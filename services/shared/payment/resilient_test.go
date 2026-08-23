package payment

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// noVault — провайдер без привязки карт. Отдельный тип нужен потому, что
// поддержка карт определяется по реализации CardVault, а не флагом.
type noVault struct {
	Gateway
}

func (n noVault) Provider() Provider { return ProviderSandbox }

func TestResilientPassesThrough(t *testing.T) {
	ctx := context.Background()
	sandbox := NewSandbox()
	resilient := NewResilient(sandbox)

	if resilient.Provider() != ProviderSandbox {
		t.Errorf("провайдер %s, ожидался %s", resilient.Provider(), ProviderSandbox)
	}

	deposit, err := resilient.Deposit(ctx, DepositRequest{
		IdempotencyKey: "dep-1", UserId: uuid.New(), Amount: 100_00, Method: MethodSBP,
	})
	if err != nil {
		t.Fatalf("пополнение: %v", err)
	}

	status, err := resilient.Status(ctx, deposit.Id)
	if err != nil {
		t.Fatalf("состояние операции: %v", err)
	}
	if status.Id != deposit.Id {
		t.Errorf("операция %s, ожидалась %s", status.Id, deposit.Id)
	}

	payout, err := resilient.Payout(ctx, PayoutRequest{
		IdempotencyKey: "pay-1", UserId: uuid.New(), Amount: 100_00,
		Method: MethodSBP, Phone: "+79001112233",
	})
	if err != nil {
		t.Fatalf("выплата: %v", err)
	}
	if payout.Id == "" {
		t.Error("операция выплаты не создана")
	}
}

func TestResilientCards(t *testing.T) {
	ctx := context.Background()
	user := uuid.New()
	resilient := NewResilient(NewSandbox())

	binding, err := resilient.Bind(ctx, user)
	if err != nil {
		t.Fatalf("привязка карты: %v", err)
	}
	if binding.Token == "" {
		t.Fatal("токен привязки не выдан")
	}

	card, err := resilient.Card(ctx, binding.Token)
	if err != nil {
		t.Fatalf("чтение карты: %v", err)
	}
	if card.Token != binding.Token {
		t.Errorf("карта %+v", card)
	}

	if err := resilient.Unbind(ctx, binding.Token); err != nil {
		t.Fatalf("отвязка карты: %v", err)
	}
	if _, err := resilient.Card(ctx, binding.Token); !errors.Is(err, ErrNotFound) {
		t.Errorf("после отвязки получено %v, ожидалась ErrNotFound", err)
	}
	if err := resilient.Unbind(ctx, binding.Token); !errors.Is(err, ErrNotFound) {
		t.Errorf("повторная отвязка вернула %v, ожидалась ErrNotFound", err)
	}
}

// TestResilientWithoutCardVault: привязка карт есть не у всякого
// провайдера, и отсутствие поддержки должно называться своим именем,
// а не имитироваться.
func TestResilientWithoutCardVault(t *testing.T) {
	ctx := context.Background()
	resilient := NewResilient(noVault{})

	if _, err := resilient.Bind(ctx, uuid.New()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("привязка вернула %v, ожидалась ErrUnsupported", err)
	}
	if _, err := resilient.Card(ctx, "token"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("чтение карты вернуло %v, ожидалась ErrUnsupported", err)
	}
	if err := resilient.Unbind(ctx, "token"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("отвязка вернула %v, ожидалась ErrUnsupported", err)
	}
}

// TestSandboxWithoutCards: тот же отказ, но от самой песочницы —
// флаг поддержки карт выключается в конфигурации стенда.
func TestSandboxWithoutCards(t *testing.T) {
	ctx := context.Background()
	sandbox := NewSandbox()
	sandbox.CardsSupported = false

	if _, err := sandbox.Bind(ctx, uuid.New()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("привязка вернула %v, ожидалась ErrUnsupported", err)
	}
	if _, err := sandbox.Card(ctx, "token"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("чтение карты вернуло %v, ожидалась ErrUnsupported", err)
	}
	if err := sandbox.Unbind(ctx, "token"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("отвязка вернула %v, ожидалась ErrUnsupported", err)
	}
}

// TestResilientOpensCircuit фиксирует главное свойство обёртки: цепь
// размыкается на недоступность провайдера, а не на его отказ в платеже —
// отказ это нормальный ответ.
func TestResilientOpensCircuit(t *testing.T) {
	ctx := context.Background()
	sandbox := NewSandbox()
	resilient := NewResilient(sandbox)
	sandbox.SetUnavailable(true)

	for i := range breakerThreshold {
		_, err := resilient.Status(ctx, "operation-"+string(rune('a'+i)))
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("попытка %d: получено %v, ожидалась ErrUnavailable", i+1, err)
		}
	}

	// Цепь разомкнута: провайдер больше не вызывается, и это видно
	// по тому, что вернувшийся в строй провайдер всё равно недоступен.
	sandbox.SetUnavailable(false)
	if _, err := resilient.Status(ctx, "operation-after"); err == nil {
		t.Error("вызов прошёл при разомкнутой цепи")
	}
}

// TestResilientKeepsRejections: отказ в платеже цепь не размыкает,
// иначе один некорректный запрос выключал бы провайдера для всех.
func TestResilientKeepsRejections(t *testing.T) {
	ctx := context.Background()
	resilient := NewResilient(NewSandbox())

	for range breakerThreshold + 1 {
		_, err := resilient.Deposit(ctx, DepositRequest{IdempotencyKey: "", UserId: uuid.Nil})
		if !errors.Is(err, ErrRejected) {
			t.Fatalf("получено %v, ожидалась ErrRejected", err)
		}
	}

	// Цепь замкнута: корректный запрос проходит.
	if _, err := resilient.Deposit(ctx, DepositRequest{
		IdempotencyKey: "dep-ok", UserId: uuid.New(), Amount: 100_00, Method: MethodSBP,
	}); err != nil {
		t.Errorf("корректный запрос отклонён: %v", err)
	}
}
