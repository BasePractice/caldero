//go:build integration

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wish/services/shared/notify"
	"wish/services/testsupport"

	"github.com/google/uuid"
)

// notifyStub принимает оповещения вместо сервиса notify. Через него тест
// узнаёт код: в ответе API кода нет и быть не должно.
type notifyStub struct {
	mu     sync.Mutex
	events []notify.PublishEvent
}

func (n *notifyStub) start(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event notify.PublishEvent
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		n.mu.Lock()
		n.events = append(n.events, event)
		n.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func (n *notifyStub) last(t *testing.T) notify.PublishEvent {
	t.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()

	if len(n.events) == 0 {
		t.Fatal("оповещений не было: код никуда не ушёл")
	}
	return n.events[len(n.events)-1]
}

func (n *notifyStub) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.events)
}

func newConfirmationService(t *testing.T, events *notifyStub) *Service {
	t.Helper()

	cfg := testsupport.Prepare(t, "users")
	cfg.OAuth2GlobalSecret = "0123456789abcdef0123456789abcdef"
	cfg.NotifyEndpoint = events.start(t)
	cfg.ServiceUserId = uuid.New()
	cfg.ConfirmationTTL = 15 * time.Minute
	cfg.ConfirmationCooldown = 0
	cfg.ConfirmationRateLimit = 3
	cfg.ConfirmationRateWindow = time.Hour
	cfg.PublicBaseURL = "https://wish.example"

	service, err := newService(context.Background(), cfg)
	if err != nil {
		t.Fatalf("не удалось создать сервис: %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return service
}

func registerUser(t *testing.T, service *Service, phone, email string) *User {
	t.Helper()

	user, err := service.db.CreateUser(context.Background(), Registration{
		Username:     "user-" + uuid.NewString()[:8],
		PasswordHash: "not-a-real-hash",
		Phone:        phone,
		Email:        email,
	})
	if err != nil {
		t.Fatalf("регистрация: %v", err)
	}
	return user
}

func TestPhoneConfirmationFlow(t *testing.T) {
	ctx := context.Background()
	events := &notifyStub{}
	service := newConfirmationService(t, events)
	user := registerUser(t, service, "+79001112233", "")

	confirmation, err := service.RequestConfirmation(ctx, user, ConfirmPhone)
	if err != nil {
		t.Fatalf("запрос кода: %v", err)
	}
	if confirmation.ExpiresAt.Before(time.Now()) {
		t.Error("код выдан уже истёкшим")
	}

	// Код уходит на контакт, а не в ответ API: иначе подтверждение
	// ничего не подтверждает.
	event := events.last(t)
	if event.Type != notify.EventConfirmationCode || event.UserId != user.Id {
		t.Fatalf("оповещение: %+v", event)
	}
	code := event.Payload["code"]
	if len(code) != codeDigits {
		t.Fatalf("код в оповещении: %q", code)
	}

	t.Run("неверный код не подтверждает", func(t *testing.T) {
		wrong := "000000"
		if wrong == code {
			wrong = "111111"
		}
		if err := service.VerifyConfirmation(ctx, user, ConfirmPhone, wrong); err != ErrWrongCode {
			t.Errorf("получено %v, ожидалась %v", err, ErrWrongCode)
		}
		current, err := service.db.GetUserById(ctx, user.Id)
		if err != nil {
			t.Fatalf("чтение профиля: %v", err)
		}
		if current.PhoneConfirmed {
			t.Error("телефон подтверждён неверным кодом")
		}
	})

	if err = service.VerifyConfirmation(ctx, user, ConfirmPhone, code); err != nil {
		t.Fatalf("подтверждение: %v", err)
	}

	confirmed, err := service.db.GetUserById(ctx, user.Id)
	if err != nil {
		t.Fatalf("чтение профиля: %v", err)
	}
	if !confirmed.PhoneConfirmed {
		t.Fatal("телефон не отмечен подтверждённым")
	}

	t.Run("повторное подтверждение отклоняется", func(t *testing.T) {
		if err := service.VerifyConfirmation(ctx, confirmed, ConfirmPhone, code); err == nil {
			t.Error("подтверждённый контакт подтверждён повторно")
		}
		if _, err := service.RequestConfirmation(ctx, confirmed, ConfirmPhone); err == nil {
			t.Error("код запрошен для подтверждённого контакта")
		}
	})
}

func TestEmailConfirmationSendsLink(t *testing.T) {
	ctx := context.Background()
	events := &notifyStub{}
	service := newConfirmationService(t, events)
	user := registerUser(t, service, "+79001112244", "user@example.com")

	if _, err := service.RequestConfirmation(ctx, user, ConfirmEmail); err != nil {
		t.Fatalf("запрос кода: %v", err)
	}

	// Токен в тридцать два байта руками не переписывают, поэтому почте
	// уходит ссылка.
	event := events.last(t)
	if event.Type != notify.EventConfirmationLink {
		t.Fatalf("тип оповещения %s", event.Type)
	}
	link := event.Payload["link"]
	if !strings.HasPrefix(link, "https://wish.example/confirm?") {
		t.Fatalf("ссылка: %q", link)
	}

	code := link[strings.Index(link, "code=")+len("code="):]
	if index := strings.Index(code, "&"); index >= 0 {
		code = code[:index]
	}
	if err := service.VerifyConfirmation(ctx, user, ConfirmEmail, code); err != nil {
		t.Fatalf("подтверждение по ссылке: %v", err)
	}

	confirmed, err := service.db.GetUserById(ctx, user.Id)
	if err != nil {
		t.Fatalf("чтение профиля: %v", err)
	}
	if !confirmed.EmailConfirmed {
		t.Error("почта не отмечена подтверждённой")
	}
	if confirmed.PhoneConfirmed {
		t.Error("подтверждение почты затронуло телефон")
	}
}

// TestConfirmationRateLimit проверяет главную защиту: без неё эндпоинт
// отправки превращается в средство рассылки за чужой счёт.
func TestConfirmationRateLimit(t *testing.T) {
	ctx := context.Background()
	events := &notifyStub{}
	service := newConfirmationService(t, events)
	user := registerUser(t, service, "+79001112255", "")

	for i := range service.cfg.ConfirmationRateLimit {
		if _, err := service.RequestConfirmation(ctx, user, ConfirmPhone); err != nil {
			t.Fatalf("запрос кода %d: %v", i, err)
		}
	}

	sent := events.count()
	if _, err := service.RequestConfirmation(ctx, user, ConfirmPhone); err == nil {
		t.Fatal("код выдан сверх предела")
	}
	if events.count() != sent {
		t.Error("сообщение ушло, хотя код не выдан")
	}
}

func TestConfirmationCooldown(t *testing.T) {
	ctx := context.Background()
	events := &notifyStub{}
	service := newConfirmationService(t, events)
	service.cfg.ConfirmationCooldown = time.Minute
	user := registerUser(t, service, "+79001112266", "")

	if _, err := service.RequestConfirmation(ctx, user, ConfirmPhone); err != nil {
		t.Fatalf("запрос кода: %v", err)
	}
	// Пауза между отправками нужна отдельно от общего предела: иначе
	// на один номер уйдёт пять сообщений подряд.
	if _, err := service.RequestConfirmation(ctx, user, ConfirmPhone); err == nil {
		t.Error("второй код выдан сразу после первого")
	}
}

func TestConfirmationAttemptsAreLimited(t *testing.T) {
	ctx := context.Background()
	events := &notifyStub{}
	service := newConfirmationService(t, events)
	user := registerUser(t, service, "+79001112277", "")

	if _, err := service.RequestConfirmation(ctx, user, ConfirmPhone); err != nil {
		t.Fatalf("запрос кода: %v", err)
	}
	code := events.last(t).Payload["code"]

	wrong := "000000"
	if wrong == code {
		wrong = "111111"
	}
	for i := range MaxAttempts {
		if err := service.VerifyConfirmation(ctx, user, ConfirmPhone, wrong); err != ErrWrongCode {
			t.Fatalf("попытка %d: получено %v", i, err)
		}
	}

	// Код сгорел: шестизначный код иначе подбирается перебором.
	if err := service.VerifyConfirmation(ctx, user, ConfirmPhone, code); err != ErrNoConfirmation {
		t.Errorf("получено %v, ожидалась %v", err, ErrNoConfirmation)
	}
}

func TestConfirmationExpires(t *testing.T) {
	ctx := context.Background()
	events := &notifyStub{}
	service := newConfirmationService(t, events)
	service.cfg.ConfirmationTTL = -time.Minute
	user := registerUser(t, service, "+79001112288", "")

	if _, err := service.RequestConfirmation(ctx, user, ConfirmPhone); err != nil {
		t.Fatalf("запрос кода: %v", err)
	}
	code := events.last(t).Payload["code"]

	if err := service.VerifyConfirmation(ctx, user, ConfirmPhone, code); err != ErrNoConfirmation {
		t.Errorf("истёкший код принят: %v", err)
	}
}

// TestConfirmationFollowsContact проверяет, что код от прежнего номера
// не подтверждает новый.
func TestConfirmationFollowsContact(t *testing.T) {
	ctx := context.Background()
	events := &notifyStub{}
	service := newConfirmationService(t, events)
	user := registerUser(t, service, "+79001112299", "")

	if _, err := service.RequestConfirmation(ctx, user, ConfirmPhone); err != nil {
		t.Fatalf("запрос кода: %v", err)
	}
	code := events.last(t).Payload["code"]

	changed := "+79001113300"
	updated, err := service.db.UpdateProfile(ctx, user.Id, ProfileUpdate{Phone: &changed})
	if err != nil {
		t.Fatalf("смена телефона: %v", err)
	}
	if updated.PhoneConfirmed {
		t.Error("смена номера не сбросила подтверждение")
	}

	if err = service.VerifyConfirmation(ctx, updated, ConfirmPhone, code); err != ErrTargetChanged {
		t.Errorf("получено %v, ожидалась %v", err, ErrTargetChanged)
	}
}

func TestConfirmationRequiresContact(t *testing.T) {
	ctx := context.Background()
	events := &notifyStub{}
	service := newConfirmationService(t, events)
	user := registerUser(t, service, "+79001113311", "")

	// Почты в профиле нет: подтверждать нечего.
	if _, err := service.RequestConfirmation(ctx, user, ConfirmEmail); err == nil {
		t.Error("код запрошен для незаполненного контакта")
	}
	if events.count() != 0 {
		t.Error("сообщение ушло, хотя контакта нет")
	}
}
