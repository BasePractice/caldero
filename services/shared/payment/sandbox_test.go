package payment

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"wish/services"
	"wish/services/shared/credit"
)

func TestSandboxIdempotency(t *testing.T) {
	ctx := context.Background()
	sandbox := NewSandbox()
	request := DepositRequest{
		IdempotencyKey: "key-1", UserId: uuid.New(), Amount: 300_00, Method: MethodSBP,
	}

	first, err := sandbox.Deposit(ctx, request)
	if err != nil {
		t.Fatalf("пополнение: %v", err)
	}
	second, err := sandbox.Deposit(ctx, request)
	if err != nil {
		t.Fatalf("повтор пополнения: %v", err)
	}
	if first.Id != second.Id {
		t.Errorf("повтор с тем же ключом создал вторую операцию: %s и %s", first.Id, second.Id)
	}
	if first.Status != StatusPending {
		t.Errorf("статус %s, ожидался %s", first.Status, StatusPending)
	}

	// Идентификатор производится из ключа: без этого тесты, сверяющие
	// состояние по идентификатору, становятся невоспроизводимыми.
	repeated, err := NewSandbox().Deposit(ctx, request)
	if err != nil {
		t.Fatalf("пополнение в новой песочнице: %v", err)
	}
	if repeated.Id != first.Id {
		t.Errorf("идентификатор недетерминирован: %s и %s", repeated.Id, first.Id)
	}
}

func TestSandboxFeeAndPayout(t *testing.T) {
	ctx := context.Background()
	sandbox := NewSandbox()
	sandbox.Fee = Fee{BasisPoints: 250, Min: 10_00}
	user := uuid.New()

	deposit, err := sandbox.Deposit(ctx, DepositRequest{
		IdempotencyKey: "d-1", UserId: user, Amount: 1_000_00, Method: MethodSBP,
	})
	if err != nil {
		t.Fatalf("пополнение: %v", err)
	}
	if want := credit.Amount(25_00); deposit.Fee != want {
		t.Errorf("комиссия %s, ожидалась %s", deposit.Fee, want)
	}

	t.Run("выплата на непривязанную карту отклоняется", func(t *testing.T) {
		_, err := sandbox.Payout(ctx, PayoutRequest{
			IdempotencyKey: "p-1", UserId: user, Amount: 100_00,
			Method: MethodCard, CardToken: "unknown",
		})
		if !errors.Is(err, ErrRejected) {
			t.Errorf("получено %v, ожидалась %v", err, ErrRejected)
		}
	})

	t.Run("выплата на привязанную карту принимается", func(t *testing.T) {
		binding, err := sandbox.Bind(ctx, user)
		if err != nil {
			t.Fatalf("привязка карты: %v", err)
		}
		card, err := sandbox.Card(ctx, binding.Token)
		if err != nil {
			t.Fatalf("чтение карты: %v", err)
		}
		if len(card.Last4) != 4 {
			t.Errorf("маска карты %q, ожидались четыре знака", card.Last4)
		}
		operation, err := sandbox.Payout(ctx, PayoutRequest{
			IdempotencyKey: "p-2", UserId: user, Amount: 100_00,
			Method: MethodCard, CardToken: binding.Token,
		})
		if err != nil {
			t.Fatalf("выплата: %v", err)
		}
		if operation.Direction != DirectionPayout {
			t.Errorf("направление %s, ожидалось %s", operation.Direction, DirectionPayout)
		}
	})
}

// TestConcurrentWebhooks проверяет главное свойство контура: одновременные
// вебхуки одной операции не проводят её дважды.
func TestConcurrentWebhooks(t *testing.T) {
	ctx := context.Background()
	store, operation := newTestOperation(t)

	const workers = 16
	var wg sync.WaitGroup
	applied := make([]bool, workers)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status := StatusSucceeded
			if i%2 == 0 {
				status = StatusFailed
			}
			event := Event{
				Id: "evt-" + strconv.Itoa(i), OperationId: operation.Id,
				Status: status, Amount: operation.Amount, OccurredAt: time.Now(),
			}
			_, err := store.Apply(ctx, event, event.ApplyTo)
			applied[i] = err == nil
		}()
	}
	wg.Wait()

	settled := 0
	for _, ok := range applied {
		if ok {
			settled++
		}
	}
	if settled != 1 {
		t.Errorf("операция завершена %d раз, ожидался ровно один", settled)
	}
}

func TestReconcileRecoversLostWebhook(t *testing.T) {
	ctx := context.Background()
	sandbox := NewSandbox()
	store := NewMemory()

	operation, err := sandbox.Deposit(ctx, DepositRequest{
		IdempotencyKey: "key-1", UserId: uuid.New(), Amount: 700_00, Method: MethodSBP,
	})
	if err != nil {
		t.Fatalf("пополнение: %v", err)
	}
	// Операция залежалась: время создания сдвинуто в прошлое, чтобы она
	// попала в окно сверки.
	operation.CreatedAt = time.Now().Add(-time.Hour)
	operation.UpdatedAt = operation.CreatedAt
	if err = store.Put(ctx, operation); err != nil {
		t.Fatalf("сохранение операции: %v", err)
	}

	// Провайдер провёл платёж, но вебхук не дошёл.
	if _, err = sandbox.Advance(operation.Id, StatusSucceeded, ""); err != nil {
		t.Fatalf("подтверждение в песочнице: %v", err)
	}

	reconciler := NewReconciler(sandbox, store, store)
	result, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatalf("сверка: %v", err)
	}
	if result.Updated != 1 {
		t.Fatalf("обновлено операций: %d, ожидалась одна (%+v)", result.Updated, result)
	}

	current, err := store.Operation(ctx, operation.Id)
	if err != nil {
		t.Fatalf("чтение операции: %v", err)
	}
	if current.Status != StatusSucceeded {
		t.Errorf("статус %s, ожидался %s", current.Status, StatusSucceeded)
	}

	t.Run("повторная сверка ничего не меняет", func(t *testing.T) {
		repeated, err := reconciler.Reconcile(ctx)
		if err != nil {
			t.Fatalf("повторная сверка: %v", err)
		}
		if repeated.Updated != 0 {
			t.Errorf("обновлено операций: %d, ожидалось ноль", repeated.Updated)
		}
	})
}

// countingGateway считает обращения к провайдеру.
type countingGateway struct {
	sandbox *Sandbox
	calls   int
}

func (g *countingGateway) Provider() Provider { return ProviderSandbox }

func (g *countingGateway) Deposit(ctx context.Context, request DepositRequest) (Operation, error) {
	g.calls++
	return g.sandbox.Deposit(ctx, request)
}

func (g *countingGateway) Payout(ctx context.Context, request PayoutRequest) (Operation, error) {
	g.calls++
	return g.sandbox.Payout(ctx, request)
}

func (g *countingGateway) Status(ctx context.Context, id string) (Operation, error) {
	g.calls++
	return g.sandbox.Status(ctx, id)
}

func TestResilientBreaker(t *testing.T) {
	ctx := context.Background()
	sandbox := NewSandbox()
	sandbox.SetUnavailable(true)
	gateway := &countingGateway{sandbox: sandbox}
	resilient := NewResilient(gateway)

	request := func(index int) DepositRequest {
		return DepositRequest{
			IdempotencyKey: "key-" + strconv.Itoa(index), UserId: uuid.New(),
			Amount: 100_00, Method: MethodSBP,
		}
	}

	for i := range breakerThreshold {
		if _, err := resilient.Deposit(ctx, request(i)); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("вызов %d: получено %v, ожидалась %v", i, err, ErrUnavailable)
		}
	}
	if gateway.calls != breakerThreshold {
		t.Fatalf("обращений к провайдеру %d, ожидалось %d", gateway.calls, breakerThreshold)
	}

	// Цепь разомкнута: провайдера больше не трогаем, иначе он
	// восстанавливается тем дольше, чем больше запросов получает.
	if _, err := resilient.Deposit(ctx, request(breakerThreshold)); !errors.Is(err, services.ErrCircuitOpen) {
		t.Errorf("получено %v, ожидалась %v", err, services.ErrCircuitOpen)
	}
	if gateway.calls != breakerThreshold {
		t.Errorf("обращений к провайдеру %d, ожидалось %d", gateway.calls, breakerThreshold)
	}
}

func TestResilientKeepsCircuitClosedOnRejection(t *testing.T) {
	ctx := context.Background()
	gateway := &countingGateway{sandbox: NewSandbox()}
	resilient := NewResilient(gateway)

	// Отказ в платеже — нормальный ответ провайдера, и цепь от него
	// размыкаться не должна: иначе одна серия неверных запросов
	// отключает платежи всем.
	for i := range breakerThreshold + 2 {
		_, err := resilient.Payout(ctx, PayoutRequest{
			IdempotencyKey: "key-" + strconv.Itoa(i), UserId: uuid.New(),
			Amount: 100_00, Method: MethodCard, CardToken: "unknown",
		})
		if !errors.Is(err, ErrRejected) {
			t.Fatalf("вызов %d: получено %v, ожидалась %v", i, err, ErrRejected)
		}
	}
	if gateway.calls != breakerThreshold+2 {
		t.Errorf("обращений к провайдеру %d, ожидалось %d", gateway.calls, breakerThreshold+2)
	}
}
