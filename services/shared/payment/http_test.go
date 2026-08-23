package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNewVerifier(t *testing.T) {
	t.Run("без секрета", func(t *testing.T) {
		// Пустой секрет означает, что подпись не проверяется вовсе:
		// принимать такую настройку молча нельзя.
		if _, err := NewVerifier("", time.Minute); err == nil {
			t.Fatal("пустой секрет принят")
		}
	})

	t.Run("неположительный допуск заменяется значением по умолчанию", func(t *testing.T) {
		verifier, err := NewVerifier(testSecret, 0)
		if err != nil {
			t.Fatalf("создание проверяющего: %v", err)
		}
		if verifier.tolerance != DefaultWebhookTolerance {
			t.Errorf("допуск %s, ожидался %s", verifier.tolerance, DefaultWebhookTolerance)
		}
	})
}

func TestVerifyRejects(t *testing.T) {
	verifier, err := NewVerifier(testSecret, time.Minute)
	if err != nil {
		t.Fatalf("создание проверяющего: %v", err)
	}
	body := []byte(`{"id":"evt-1"}`)
	now := time.Now()

	tests := []struct {
		name      string
		timestamp time.Time
		signature string
		want      error
	}{
		{
			name:      "без отметки времени",
			signature: verifier.Sign(body, now),
			want:      ErrStaleSignature,
		},
		{
			// Без окна свежести перехваченный вебхук доставляется повторно
			// через сутки и снова считается подлинным.
			name:      "просроченная отметка",
			timestamp: now.Add(-time.Hour),
			signature: verifier.Sign(body, now.Add(-time.Hour)),
			want:      ErrStaleSignature,
		},
		{
			name:      "отметка из будущего",
			timestamp: now.Add(time.Hour),
			signature: verifier.Sign(body, now.Add(time.Hour)),
			want:      ErrStaleSignature,
		},
		{
			name:      "подпись не шестнадцатеричная",
			timestamp: now,
			signature: "не-подпись",
			want:      ErrInvalidSignature,
		},
		{
			name:      "чужая подпись",
			timestamp: now,
			signature: "00ff",
			want:      ErrInvalidSignature,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := verifier.Verify(body, test.timestamp, test.signature); !errors.Is(err, test.want) {
				t.Errorf("получено %v, ожидалась %v", err, test.want)
			}
		})
	}
}

func TestEventValidate(t *testing.T) {
	valid := Event{
		Id: "evt-1", OperationId: "op-1", Provider: ProviderSandbox,
		Status: StatusSucceeded, Amount: 500_00, OccurredAt: time.Now(),
	}

	tests := []struct {
		name    string
		change  func(*Event)
		wantErr bool
	}{
		{"корректное событие", func(*Event) {}, false},
		{"состояние ожидания", func(e *Event) { e.Status = StatusPending }, false},
		{"отказ", func(e *Event) { e.Status = StatusFailed }, false},
		{"без идентификатора", func(e *Event) { e.Id = "" }, true},
		{"без операции", func(e *Event) { e.OperationId = "" }, true},
		{"неизвестное состояние", func(e *Event) { e.Status = "WHATEVER" }, true},
		{"без времени события", func(e *Event) { e.OccurredAt = time.Time{} }, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := valid
			test.change(&event)

			err := event.Validate()
			if test.wantErr && err == nil {
				t.Error("событие принято, ожидался отказ")
			}
			if !test.wantErr && err != nil {
				t.Errorf("событие отклонено: %v", err)
			}
		})
	}
}

func TestWebhookHandlerRejections(t *testing.T) {
	verifier, err := NewVerifier(testSecret, time.Minute)
	if err != nil {
		t.Fatalf("создание проверяющего: %v", err)
	}
	store, operation := newTestOperation(t)
	handler := NewWebhookHandler(verifier, store)

	sign := func(body []byte, timestamp time.Time) *http.Request {
		request := httptest.NewRequest(http.MethodPost, "/webhooks/payment", bytes.NewReader(body))
		request.Header.Set(DefaultSignatureHeader, verifier.Sign(body, timestamp))
		request.Header.Set(DefaultTimestampHeader, strconv.FormatInt(timestamp.Unix(), 10))
		return request
	}
	do := func(request *http.Request) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	t.Run("метод, отличный от POST", func(t *testing.T) {
		recorder := do(httptest.NewRequest(http.MethodGet, "/webhooks/payment", nil))
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusMethodNotAllowed)
		}
		if recorder.Header().Get("Allow") != http.MethodPost {
			t.Error("не указан допустимый метод")
		}
	})

	t.Run("тело больше предела", func(t *testing.T) {
		// Размер тела задаёт чужой сервис, и без ограничения он задаёт
		// и потребление памяти.
		body := []byte(`{"id":"` + strings.Repeat("x", int(MaxWebhookBody)+1) + `"}`)
		recorder := do(sign(body, time.Now()))
		if recorder.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusRequestEntityTooLarge)
		}
	})

	t.Run("нечитаемая отметка времени", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/webhooks/payment",
			strings.NewReader(`{}`))
		request.Header.Set(DefaultTimestampHeader, "недавно")
		if recorder := do(request); recorder.Code != http.StatusBadRequest {
			t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusBadRequest)
		}
	})

	t.Run("отметка времени отсутствует", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/webhooks/payment",
			strings.NewReader(`{}`))
		if recorder := do(request); recorder.Code != http.StatusBadRequest {
			t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusBadRequest)
		}
	})

	t.Run("тело не разбирается", func(t *testing.T) {
		recorder := do(sign([]byte(`не json`), time.Now()))
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusBadRequest)
		}
	})

	t.Run("событие не проходит проверку", func(t *testing.T) {
		body, err := json.Marshal(Event{OperationId: operation.Id, Status: StatusSucceeded})
		if err != nil {
			t.Fatalf("кодирование события: %v", err)
		}
		recorder := do(sign(body, time.Now()))
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("код ответа %d, ожидался %d (%s)", recorder.Code, http.StatusBadRequest, recorder.Body)
		}
	})

	t.Run("сбой хранилища требует повтора", func(t *testing.T) {
		// Иначе операция навсегда останется незавершённой: провайдер
		// считает доставку успешной и больше не повторит.
		failing := NewWebhookHandler(verifier, failingStore{err: errors.New("connection refused")})
		body, err := json.Marshal(Event{
			Id: "evt-fail", OperationId: operation.Id, Provider: ProviderSandbox,
			Status: StatusSucceeded, Amount: operation.Amount, OccurredAt: time.Now(),
		})
		if err != nil {
			t.Fatalf("кодирование события: %v", err)
		}

		recorder := httptest.NewRecorder()
		failing.ServeHTTP(recorder, sign(body, time.Now()))
		if recorder.Code != http.StatusServiceUnavailable {
			t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusServiceUnavailable)
		}
	})
}

// failingStore изображает недоступное хранилище операций.
type failingStore struct {
	err error
}

func (f failingStore) Apply(context.Context, Event, func(Operation) (Operation, error)) (Operation, error) {
	return Operation{}, f.err
}
