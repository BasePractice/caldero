package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
)

const testSecret = "webhook-secret"

func newTestOperation(t *testing.T) (*Memory, Operation) {
	t.Helper()

	operation := Operation{
		Provider:       ProviderSandbox,
		Id:             "op-1",
		UserId:         uuid.New(),
		IdempotencyKey: "key-1",
		Direction:      DirectionDeposit,
		Method:         MethodSBP,
		Status:         StatusPending,
		Amount:         500_00,
		CreatedAt:      time.Now().Add(-time.Hour),
		UpdatedAt:      time.Now().Add(-time.Hour),
	}
	store := NewMemory()
	if err := store.Put(context.Background(), operation); err != nil {
		t.Fatalf("сохранение операции: %v", err)
	}
	return store, operation
}

func TestVerifierSignature(t *testing.T) {
	verifier, err := NewVerifier(testSecret, time.Minute)
	if err != nil {
		t.Fatalf("создание проверяющего: %v", err)
	}
	body := []byte(`{"id":"evt-1"}`)
	now := time.Now()

	t.Run("своя подпись проходит", func(t *testing.T) {
		if err := verifier.Verify(body, now, verifier.Sign(body, now)); err != nil {
			t.Errorf("подпись отвергнута: %v", err)
		}
	})

	t.Run("подменённое тело не проходит", func(t *testing.T) {
		signature := verifier.Sign(body, now)
		err := verifier.Verify([]byte(`{"id":"evt-2"}`), now, signature)
		if !errors.Is(err, ErrInvalidSignature) {
			t.Errorf("получено %v, ожидалась %v", err, ErrInvalidSignature)
		}
	})

	t.Run("подменённое время не проходит", func(t *testing.T) {
		signature := verifier.Sign(body, now)
		err := verifier.Verify(body, now.Add(-30*time.Second), signature)
		if !errors.Is(err, ErrInvalidSignature) {
			t.Errorf("получено %v, ожидалась %v", err, ErrInvalidSignature)
		}
	})

	t.Run("устаревший вебхук не принимается", func(t *testing.T) {
		old := now.Add(-time.Hour)
		err := verifier.Verify(body, old, verifier.Sign(body, old))
		if !errors.Is(err, ErrStaleSignature) {
			t.Errorf("получено %v, ожидалась %v", err, ErrStaleSignature)
		}
	})

	t.Run("чужой секрет не проходит", func(t *testing.T) {
		other, err := NewVerifier("another-secret", time.Minute)
		if err != nil {
			t.Fatalf("создание проверяющего: %v", err)
		}
		err = verifier.Verify(body, now, other.Sign(body, now))
		if !errors.Is(err, ErrInvalidSignature) {
			t.Errorf("получено %v, ожидалась %v", err, ErrInvalidSignature)
		}
	})
}

func TestEventApply(t *testing.T) {
	ctx := context.Background()
	store, operation := newTestOperation(t)

	success := Event{
		Id: "evt-success", OperationId: operation.Id, Provider: ProviderSandbox,
		Status: StatusSucceeded, Amount: operation.Amount, OccurredAt: time.Now(),
	}

	updated, err := store.Apply(ctx, success, success.ApplyTo)
	if err != nil {
		t.Fatalf("применение события: %v", err)
	}
	if updated.Status != StatusSucceeded {
		t.Fatalf("статус %s, ожидался %s", updated.Status, StatusSucceeded)
	}

	t.Run("повтор вебхука не проводит платёж дважды", func(t *testing.T) {
		_, err := store.Apply(ctx, success, success.ApplyTo)
		if !errors.Is(err, ErrEventIgnored) {
			t.Errorf("получено %v, ожидалась %v", err, ErrEventIgnored)
		}
	})

	t.Run("опоздавший отказ не отменяет проведённый платёж", func(t *testing.T) {
		failure := Event{
			Id: "evt-failure", OperationId: operation.Id, Provider: ProviderSandbox,
			Status: StatusFailed, Amount: operation.Amount,
			OccurredAt: time.Now().Add(-time.Minute),
		}
		if _, err := store.Apply(ctx, failure, failure.ApplyTo); !errors.Is(err, ErrEventIgnored) {
			t.Errorf("получено %v, ожидалась %v", err, ErrEventIgnored)
		}
		current, err := store.Operation(ctx, operation.Id)
		if err != nil {
			t.Fatalf("чтение операции: %v", err)
		}
		if current.Status != StatusSucceeded {
			t.Errorf("статус %s, ожидался %s", current.Status, StatusSucceeded)
		}
	})
}

func TestEventApplyRejectsForeignAmount(t *testing.T) {
	ctx := context.Background()
	store, operation := newTestOperation(t)

	event := Event{
		Id: "evt-foreign", OperationId: operation.Id, Provider: ProviderSandbox,
		Status: StatusSucceeded, Amount: operation.Amount * 2, OccurredAt: time.Now(),
	}
	if _, err := store.Apply(ctx, event, event.ApplyTo); err == nil {
		t.Fatal("событие с чужой суммой применено")
	}

	current, err := store.Operation(ctx, operation.Id)
	if err != nil {
		t.Fatalf("чтение операции: %v", err)
	}
	if current.Status != StatusPending {
		t.Errorf("статус %s, ожидался %s", current.Status, StatusPending)
	}
}

func TestEventApplyIgnoresStaleOrder(t *testing.T) {
	ctx := context.Background()
	store, operation := newTestOperation(t)

	// Вебхуки приходят не в том порядке, в котором произошли события:
	// сначала подтверждение, следом опоздавшее «ещё в обработке».
	success := Event{
		Id: "evt-2", OperationId: operation.Id, Status: StatusSucceeded,
		Amount: operation.Amount, OccurredAt: time.Now(),
	}
	if _, err := store.Apply(ctx, success, success.ApplyTo); err != nil {
		t.Fatalf("применение подтверждения: %v", err)
	}

	pending := Event{
		Id: "evt-1", OperationId: operation.Id, Status: StatusPending,
		Amount: operation.Amount, OccurredAt: time.Now().Add(-time.Minute),
	}
	if _, err := store.Apply(ctx, pending, pending.ApplyTo); !errors.Is(err, ErrEventIgnored) {
		t.Errorf("получено %v, ожидалась %v", err, ErrEventIgnored)
	}
}

func TestWebhookHandler(t *testing.T) {
	verifier, err := NewVerifier(testSecret, time.Minute)
	if err != nil {
		t.Fatalf("создание проверяющего: %v", err)
	}
	store, operation := newTestOperation(t)
	handler := NewWebhookHandler(verifier, store)

	event := Event{
		Id: "evt-1", OperationId: operation.Id, Provider: ProviderSandbox,
		Status: StatusSucceeded, Amount: operation.Amount, OccurredAt: time.Now(),
	}
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("кодирование события: %v", err)
	}

	post := func(body []byte, signature string, timestamp time.Time) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/webhooks/payment", bytes.NewReader(body))
		request.Header.Set(DefaultSignatureHeader, signature)
		request.Header.Set(DefaultTimestampHeader, strconv.FormatInt(timestamp.Unix(), 10))
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	t.Run("подписанный вебхук применяется", func(t *testing.T) {
		now := time.Now()
		if code := post(body, verifier.Sign(body, now), now).Code; code != http.StatusOK {
			t.Fatalf("код ответа %d, ожидался %d", code, http.StatusOK)
		}
		current, err := store.Operation(context.Background(), operation.Id)
		if err != nil {
			t.Fatalf("чтение операции: %v", err)
		}
		if current.Status != StatusSucceeded {
			t.Errorf("статус %s, ожидался %s", current.Status, StatusSucceeded)
		}
	})

	t.Run("повтор доставки — успех, а не ошибка", func(t *testing.T) {
		now := time.Now()
		// Ответь обработчик ошибкой, провайдер повторял бы доставку
		// до конца срока ретраев.
		if code := post(body, verifier.Sign(body, now), now).Code; code != http.StatusOK {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusOK)
		}
	})

	t.Run("вебхук без подписи отвергается", func(t *testing.T) {
		now := time.Now()
		if code := post(body, "", now).Code; code != http.StatusBadRequest {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusBadRequest)
		}
	})

	t.Run("вебхук неизвестной операции требует повтора", func(t *testing.T) {
		unknown := event
		unknown.Id = "evt-unknown"
		unknown.OperationId = "op-unknown"
		encoded, err := json.Marshal(unknown)
		if err != nil {
			t.Fatalf("кодирование события: %v", err)
		}
		now := time.Now()
		if code := post(encoded, verifier.Sign(encoded, now), now).Code; code != http.StatusServiceUnavailable {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusServiceUnavailable)
		}
	})

	t.Run("метод, кроме POST, не принимается", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/webhooks/payment", nil)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusMethodNotAllowed)
		}
	})
}
