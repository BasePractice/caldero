package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestClientDisabled фиксирует главное свойство клиента: без настроенного
// адреса или служебного идентификатора публикация молча ничего не делает.
// Иначе каждый сервис в конфигурации без оповещений падал бы на ошибке.
func TestClientDisabled(t *testing.T) {
	tests := []struct {
		name      string
		endpoint  string
		serviceId uuid.UUID
	}{
		{"пустой адрес", "", uuid.New()},
		{"без служебного идентификатора", "http://notify", uuid.Nil},
		{"ни адреса, ни идентификатора", "", uuid.Nil},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := NewClient(test.endpoint, test.serviceId)
			if client.Enabled() {
				t.Fatal("клиент считает себя настроенным")
			}
			// Событие заведомо некорректное: выключенный клиент не должен
			// доходить даже до проверки.
			if err := client.Publish(t.Context(), PublishEvent{}); err != nil {
				t.Errorf("выключенный клиент вернул ошибку: %v", err)
			}
		})
	}
}

// TestClientNilDisabled: сервисы держат клиента как *Client и не проверяют
// его на nil перед публикацией.
func TestClientNilDisabled(t *testing.T) {
	var client *Client
	if client.Enabled() {
		t.Fatal("нулевой клиент считает себя настроенным")
	}
	if err := client.Publish(t.Context(), PublishEvent{}); err != nil {
		t.Errorf("нулевой клиент вернул ошибку: %v", err)
	}
}

func TestClientPublish(t *testing.T) {
	serviceId := uuid.New()
	event := PublishEvent{
		UserId:   uuid.New(),
		Type:     EventWishlistItemReserved,
		Payload:  map[string]string{"item": "чайник"},
		DedupKey: "item-42",
	}

	var got PublishEvent
	var path, authorized, roles, contentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		authorized = r.Header.Get("X-Authorized-Id")
		roles = r.Header.Get("X-Roles")
		contentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("разбор тела запроса: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client := NewClient(server.URL, serviceId)
	if !client.Enabled() {
		t.Fatal("клиент с адресом и идентификатором считает себя выключенным")
	}
	if err := client.Publish(t.Context(), event); err != nil {
		t.Fatalf("публикация: %v", err)
	}

	if path != "/notify/events" {
		t.Errorf("путь %q, ожидался /notify/events", path)
	}
	if authorized != serviceId.String() {
		t.Errorf("X-Authorized-Id %q, ожидался %q", authorized, serviceId)
	}
	if roles != "operator" {
		t.Errorf("X-Roles %q, ожидалась роль оператора", roles)
	}
	if contentType != "application/json" {
		t.Errorf("Content-Type %q, ожидался application/json", contentType)
	}
	if got.UserId != event.UserId || got.Type != event.Type || got.DedupKey != event.DedupKey {
		t.Errorf("сервис получил %+v, ожидалось %+v", got, event)
	}
	if got.Payload["item"] != "чайник" {
		t.Errorf("подстановки не доехали: %+v", got.Payload)
	}
}

func TestClientPublishInvalidEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("некорректное событие ушло в сервис")
	}))
	defer server.Close()

	client := NewClient(server.URL, uuid.New())
	err := client.Publish(t.Context(), PublishEvent{Type: EventPaymentSettled})
	if err == nil {
		t.Fatal("событие без получателя принято")
	}
	if !strings.Contains(err.Error(), "invalid event") {
		t.Errorf("ошибка %q не называет причину", err)
	}
}

func TestClientPublishRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "too many events", http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := NewClient(server.URL, uuid.New())
	err := client.Publish(t.Context(), PublishEvent{UserId: uuid.New(), Type: EventPaymentSettled})
	if err == nil {
		t.Fatal("отказ сервиса не превратился в ошибку")
	}
	// Тело ответа попадает в ошибку: без него причина отказа теряется.
	if !strings.Contains(err.Error(), "too many events") {
		t.Errorf("ошибка %q не содержит ответа сервиса", err)
	}
}

func TestClientPublishUnreachable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := server.URL
	server.Close()

	client := NewClient(endpoint, uuid.New())
	err := client.Publish(t.Context(), PublishEvent{UserId: uuid.New(), Type: EventPaymentSettled})
	if err == nil {
		t.Fatal("недоступный сервис не превратился в ошибку")
	}
}

// TestClientPublishBadEndpoint проверяет ветку сборки запроса: адрес
// приходит из конфигурации и может быть каким угодно.
func TestClientPublishBadEndpoint(t *testing.T) {
	client := NewClient("http://\x7f", uuid.New())
	err := client.Publish(context.Background(), PublishEvent{UserId: uuid.New(), Type: EventPaymentSettled})
	if err == nil {
		t.Fatal("некорректный адрес принят")
	}
}
