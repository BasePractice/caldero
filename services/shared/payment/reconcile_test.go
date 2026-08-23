package payment

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"wish/services"
	"wish/services/shared/credit"
)

// stubGateway отвечает заранее заданным состоянием: сверка спрашивает
// у провайдера, что он думает об операции, и проверять нужно именно
// разбор его ответа.
type stubGateway struct {
	Gateway
	status  Status
	err     error
	asked   []string
	amount  credit.Amount
	failure string
}

func (s *stubGateway) Provider() Provider { return ProviderSandbox }

func (s *stubGateway) Status(_ context.Context, id string) (Operation, error) {
	s.asked = append(s.asked, id)
	if s.err != nil {
		return Operation{}, s.err
	}
	return Operation{
		Provider: ProviderSandbox, Id: id, Status: s.status,
		Amount: s.amount, FailureReason: s.failure, UpdatedAt: time.Now(),
	}, nil
}

// stale кладёт в хранилище операцию, залежавшуюся в незавершённом состоянии.
func stale(t *testing.T, store *Memory, id string, amount credit.Amount) Operation {
	t.Helper()
	operation := Operation{
		Provider: ProviderSandbox, Id: id, UserId: uuid.New(),
		IdempotencyKey: "key-" + id, Direction: DirectionDeposit, Method: MethodSBP,
		Status: StatusPending, Amount: amount,
		CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now().Add(-time.Hour),
	}
	if err := store.Put(context.Background(), operation); err != nil {
		t.Fatalf("сохранение операции: %v", err)
	}
	return operation
}

// Сквозной сценарий восстановления после недошедшего вебхука проверяется
// в TestReconcileRecoversLostWebhook; здесь — ветки, до которых он не доходит.
func TestReconcileSkipsMatchingStatus(t *testing.T) {
	ctx := context.Background()
	store := NewMemory()
	stale(t, store, "op-1", 500_00)

	result, err := NewReconciler(&stubGateway{status: StatusPending, amount: 500_00}, store, store).
		Reconcile(ctx)
	if err != nil {
		t.Fatalf("сверка: %v", err)
	}
	if result.Checked != 1 || result.Updated != 0 {
		t.Errorf("итог сверки %+v", result)
	}
}

// TestReconcileStopsOnUnavailable: недоступность провайдера означает,
// что и остальные вызовы не пройдут — добивать его бессмысленно.
func TestReconcileStopsOnUnavailable(t *testing.T) {
	ctx := context.Background()
	store := NewMemory()
	stale(t, store, "op-1", 500_00)
	stale(t, store, "op-2", 500_00)

	tests := []struct {
		name string
		err  error
	}{
		{"провайдер недоступен", ErrUnavailable},
		{"цепь разомкнута", services.ErrCircuitOpen},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gateway := &stubGateway{err: test.err}
			result, err := NewReconciler(gateway, store, store).Reconcile(ctx)
			if !errors.Is(err, test.err) {
				t.Fatalf("получено %v, ожидалось %v", err, test.err)
			}
			if result.Checked != 1 {
				t.Errorf("проверено %d операций: проход не остановился", result.Checked)
			}
		})
	}
}

// TestReconcileContinuesAfterSingleFailure: ошибка по отдельной операции
// не повод оставить остальные незавершёнными.
func TestReconcileContinuesAfterSingleFailure(t *testing.T) {
	ctx := context.Background()
	store := NewMemory()
	stale(t, store, "op-1", 500_00)
	stale(t, store, "op-2", 500_00)

	gateway := &stubGateway{err: errors.New("operation not found at provider")}
	result, err := NewReconciler(gateway, store, store).Reconcile(ctx)
	if err != nil {
		t.Fatalf("сверка: %v", err)
	}
	if result.Checked != 2 || result.Failed != 2 {
		t.Errorf("итог сверки %+v", result)
	}
}

// TestReconcileRejectsForeignAmount: расхождение в сумме — это не то,
// что можно молча принять от провайдера.
func TestReconcileRejectsForeignAmount(t *testing.T) {
	ctx := context.Background()
	store := NewMemory()
	stale(t, store, "op-1", 500_00)

	gateway := &stubGateway{status: StatusSucceeded, amount: 100_00}
	result, err := NewReconciler(gateway, store, store).Reconcile(ctx)
	if err != nil {
		t.Fatalf("сверка: %v", err)
	}
	if result.Failed != 1 || result.Updated != 0 {
		t.Errorf("итог сверки %+v: чужая сумма принята", result)
	}
}

// failingSource изображает недоступное хранилище: без списка операций
// сверять нечего, и это ошибка прохода, а не пустой результат.
type failingSource struct {
	err error
}

func (f failingSource) Unsettled(context.Context, time.Duration, int) ([]Operation, error) {
	return nil, f.err
}

func TestReconcileSourceFailure(t *testing.T) {
	store := NewMemory()
	reconciler := NewReconciler(&stubGateway{}, store, failingSource{err: errors.New("connection refused")})

	if _, err := reconciler.Reconcile(context.Background()); err == nil {
		t.Fatal("недоступное хранилище принято за пустой список")
	}
}

func TestReconcileStopsOnCancelledContext(t *testing.T) {
	store := NewMemory()
	stale(t, store, "op-1", 500_00)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := NewReconciler(&stubGateway{status: StatusSucceeded, amount: 500_00}, store, store).
		Reconcile(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("получено %v, ожидалась отмена контекста", err)
	}
}

// TestUnsettledLimit: провайдер ограничивает частоту запросов, и разобрать
// накопившееся за один проход всё равно нельзя.
func TestUnsettledLimit(t *testing.T) {
	ctx := context.Background()
	store := NewMemory()
	for i := range 5 {
		stale(t, store, "op-"+string(rune('a'+i)), 100_00)
	}

	unsettled, err := store.Unsettled(ctx, time.Minute, 2)
	if err != nil {
		t.Fatalf("чтение незавершённых: %v", err)
	}
	if len(unsettled) != 2 {
		t.Errorf("отдано %d операций, ожидалось 2", len(unsettled))
	}
}

func TestMemoryErrors(t *testing.T) {
	ctx := context.Background()
	store := NewMemory()

	t.Run("операция без идентификатора", func(t *testing.T) {
		if err := store.Put(ctx, Operation{}); err == nil {
			t.Error("операция без идентификатора сохранена")
		}
	})

	t.Run("неизвестная операция", func(t *testing.T) {
		if _, err := store.Operation(ctx, "нет-такой"); !errors.Is(err, ErrNotFound) {
			t.Errorf("получено %v, ожидалась ErrNotFound", err)
		}
	})
}
