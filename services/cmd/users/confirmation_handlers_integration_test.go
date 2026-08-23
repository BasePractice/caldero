//go:build integration

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wish/services/testsupport"

	"github.com/google/uuid"
)

// newConfirmationHandlerService поднимает сервис с настоящей базой,
// заглушкой оповещений и заведённым клиентом: подтверждения доступны
// только владельцу токена.
func newConfirmationHandlerService(t *testing.T, events *notifyStub) (*Service, http.Handler) {
	t.Helper()

	cfg := testsupport.Prepare(t, "users")
	cfg.OAuth2GlobalSecret = "0123456789abcdef0123456789abcdef"
	cfg.AdminToken = adminToken
	cfg.NotifyEndpoint = events.start(t)
	cfg.ServiceUserId = uuid.New()
	cfg.ConfirmationTTL = 15 * time.Minute
	cfg.ConfirmationCooldown = 0
	cfg.ConfirmationRateLimit = 2
	cfg.ConfirmationRateWindow = time.Hour
	cfg.PublicBaseURL = "https://wish.example"

	service, err := newService(context.Background(), cfg)
	if err != nil {
		t.Fatalf("не удалось создать сервис: %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return service, registerHttpHandlers(service)
}

func postJSON(handler http.Handler, path, body, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// TestConfirmationOverHTTP проходит подтверждение телефона через API целиком:
// запрос кода, неверный код, верный код и отражение результата в профиле.
func TestConfirmationOverHTTP(t *testing.T) {
	events := &notifyStub{}
	_, handler := newConfirmationHandlerService(t, events)
	clientId := createClient(t, handler)

	username := "user-" + uuid.NewString()[:8]
	registerViaAPI(t, handler, username, "+79003330011")
	token := tokenFor(t, handler, clientId, username)

	recorder := postJSON(handler, "/profile/confirmations", `{"kind":"PHONE"}`, token)
	if recorder.Code != http.StatusOK {
		t.Fatalf("запрос кода: %d (%s)", recorder.Code, recorder.Body)
	}
	var issued struct {
		Kind      string    `json:"kind"`
		ExpiresAt time.Time `json:"expires_at"`
		Attempts  int       `json:"attempts"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &issued); err != nil {
		t.Fatalf("разбор ответа: %v", err)
	}
	if issued.Kind != string(ConfirmPhone) || issued.Attempts != MaxAttempts {
		t.Errorf("ответ: %+v", issued)
	}
	// Сам код в ответе не возвращается: он должен прийти на контакт,
	// иначе подтверждение ничего не подтверждает.
	if strings.Contains(recorder.Body.String(), events.last(t).Payload["code"]) {
		t.Errorf("код вернулся в ответе API: %s", recorder.Body)
	}
	code := events.last(t).Payload["code"]

	t.Run("неверный код отклоняется", func(t *testing.T) {
		wrong := "000000"
		if wrong == code {
			wrong = "111111"
		}
		recorder := postJSON(handler, "/profile/confirmations/verify",
			`{"kind":"PHONE","code":"`+wrong+`"}`, token)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("код ответа %d, ожидался %d (%s)", recorder.Code, http.StatusBadRequest, recorder.Body)
		}
	})

	verified := postJSON(handler, "/profile/confirmations/verify",
		`{"kind":"PHONE","code":"`+code+`"}`, token)
	if verified.Code != http.StatusOK {
		t.Fatalf("подтверждение: %d (%s)", verified.Code, verified.Body)
	}
	var profile map[string]any
	if err := json.Unmarshal(verified.Body.Bytes(), &profile); err != nil {
		t.Fatalf("разбор ответа: %v", err)
	}
	if profile["phone_confirmed"] != true {
		t.Errorf("профиль не отражает подтверждение: %v", profile)
	}

	t.Run("подтверждённый контакт не подтверждается второй раз", func(t *testing.T) {
		recorder := postJSON(handler, "/profile/confirmations", `{"kind":"PHONE"}`, token)
		if recorder.Code != http.StatusConflict {
			t.Errorf("код ответа %d, ожидался %d (%s)", recorder.Code, http.StatusConflict, recorder.Body)
		}
	})
}

func TestConfirmationHandlerErrors(t *testing.T) {
	events := &notifyStub{}
	_, handler := newConfirmationHandlerService(t, events)
	clientId := createClient(t, handler)

	username := "user-" + uuid.NewString()[:8]
	registerViaAPI(t, handler, username, "+79003330022")
	token := tokenFor(t, handler, clientId, username)

	tests := []struct {
		name  string
		path  string
		body  string
		token string
		want  int
	}{
		{
			name: "без токена",
			path: "/profile/confirmations",
			body: `{"kind":"PHONE"}`,
			want: http.StatusUnauthorized,
		},
		{
			name:  "нечитаемое тело",
			path:  "/profile/confirmations",
			body:  `{"kind":`,
			token: token,
			want:  http.StatusBadRequest,
		},
		{
			name:  "неизвестный вид контакта",
			path:  "/profile/confirmations",
			body:  `{"kind":"TELEGRAM"}`,
			token: token,
			want:  http.StatusBadRequest,
		},
		{
			name:  "нечитаемое тело проверки",
			path:  "/profile/confirmations/verify",
			body:  `{"code":`,
			token: token,
			want:  http.StatusBadRequest,
		},
		{
			name:  "неизвестный вид контакта при проверке",
			path:  "/profile/confirmations/verify",
			body:  `{"kind":"TELEGRAM","code":"123456"}`,
			token: token,
			want:  http.StatusBadRequest,
		},
		{
			name:  "пустой код",
			path:  "/profile/confirmations/verify",
			body:  `{"kind":"PHONE","code":""}`,
			token: token,
			want:  http.StatusBadRequest,
		},
		{
			name:  "проверка без выданного кода",
			path:  "/profile/confirmations/verify",
			body:  `{"kind":"PHONE","code":"123456"}`,
			token: token,
			want:  http.StatusConflict,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := postJSON(handler, test.path, test.body, test.token)
			if recorder.Code != test.want {
				t.Errorf("код ответа %d, ожидался %d (%s)", recorder.Code, test.want, recorder.Body)
			}
		})
	}
}

// TestConfirmationRateLimitOverHTTP: без ограничения частоты отправка кода
// становится способом слать сообщения на чужой номер за наш счёт.
func TestConfirmationRateLimitOverHTTP(t *testing.T) {
	events := &notifyStub{}
	_, handler := newConfirmationHandlerService(t, events)
	clientId := createClient(t, handler)

	username := "user-" + uuid.NewString()[:8]
	registerViaAPI(t, handler, username, "+79003330033")
	token := tokenFor(t, handler, clientId, username)

	// Предел выставлен в два кода на окно.
	for i := range 2 {
		recorder := postJSON(handler, "/profile/confirmations", `{"kind":"PHONE"}`, token)
		if recorder.Code != http.StatusOK {
			t.Fatalf("запрос %d: %d (%s)", i+1, recorder.Code, recorder.Body)
		}
	}

	recorder := postJSON(handler, "/profile/confirmations", `{"kind":"PHONE"}`, token)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("код ответа %d, ожидался %d (%s)", recorder.Code, http.StatusTooManyRequests, recorder.Body)
	}
	// Ограничение частоты — не ошибка запроса: клиенту нужно подождать,
	// и он должен знать сколько.
	if recorder.Header().Get("Retry-After") == "" {
		t.Error("не указано, когда можно повторить")
	}
}

// TestConfirmationWithoutContact: подтверждать нечего, если контакт
// не заполнен в профиле.
func TestConfirmationWithoutContact(t *testing.T) {
	events := &notifyStub{}
	service, _ := newConfirmationHandlerService(t, events)

	user, err := service.db.CreateUser(context.Background(), Registration{
		Username:     "user-" + uuid.NewString()[:8],
		PasswordHash: "not-a-real-hash",
		Phone:        "+79003330044",
	})
	if err != nil {
		t.Fatalf("регистрация: %v", err)
	}

	if _, err := service.RequestConfirmation(context.Background(), user, ConfirmEmail); err == nil {
		t.Fatal("код запрошен для незаполненного контакта")
	}
}
