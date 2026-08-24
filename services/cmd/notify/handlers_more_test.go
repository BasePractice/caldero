package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"wish/services/shared/notify"

	"github.com/google/uuid"
)

// failingDatabase отвечает ошибкой на всё, что нужно обработчикам:
// недоступная база не должна превращаться в успешный ответ.
type failingDatabase struct {
	Database
	err error
}

func (f *failingDatabase) Messages(context.Context, uuid.UUID, int64, int) ([]notify.Message, error) {
	return nil, f.err
}

func (f *failingDatabase) Preferences(context.Context, uuid.UUID) ([]notify.Preference, error) {
	return nil, f.err
}

func (f *failingDatabase) SetPreference(context.Context, uuid.UUID, notify.Preference) error {
	return f.err
}

func (f *failingDatabase) StartMessengerBinding(context.Context, notify.Channel, uuid.UUID, []byte, time.Time) error {
	return f.err
}

func (f *failingDatabase) MessengerBinding(context.Context, notify.Channel, uuid.UUID) (MessengerBinding, error) {
	return MessengerBinding{}, f.err
}

// StartMessengerBinding дополняет fakeDatabase из тестов доставки:
// привязку начинает обработчик, а не диспетчер.
func (f *fakeDatabase) StartMessengerBinding(
	_ context.Context,
	_ notify.Channel,
	_ uuid.UUID,
	codeHash []byte,
	_ time.Time,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startedHash = codeHash
	return nil
}

func TestMessagesParamValidation(t *testing.T) {
	handler := newTestAPI(&fakeDatabase{}, NewHub())
	user := uuid.New()

	tests := []struct {
		name  string
		query string
		want  int
	}{
		{"нечитаемый курсор", "?after=позже", http.StatusBadRequest},
		{"нечитаемый предел", "?limit=много", http.StatusBadRequest},
		{"неположительный предел", "?limit=0", http.StatusBadRequest},
		{"нечитаемое ожидание", "?wait=долго", http.StatusBadRequest},
		{"отрицательное ожидание", "?wait=-1", http.StatusBadRequest},
		// Предел страницы ограничен сверху, но запрос с большим значением
		// это не ошибка: он просто урезается.
		{"предел больше максимума", "?limit=1000", http.StatusOK},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, authorized(
				httptest.NewRequest(http.MethodGet, "/notify/messages"+test.query, nil), user, ""))
			if recorder.Code != test.want {
				t.Errorf("код ответа %d, ожидался %d (%s)", recorder.Code, test.want, recorder.Body)
			}
		})
	}
}

func TestHandlerDatabaseFailures(t *testing.T) {
	failing := &failingDatabase{err: errors.New("connection refused")}
	handler := newTestAPI(failing, NewHub())
	user := uuid.New()

	t.Run("лента", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, authorized(
			httptest.NewRequest(http.MethodGet, "/notify/messages", nil), user, ""))
		if recorder.Code != http.StatusInternalServerError {
			t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusInternalServerError)
		}
	})

	t.Run("настройки", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, authorized(
			httptest.NewRequest(http.MethodGet, "/notify/preferences", nil), user, ""))
		if recorder.Code != http.StatusInternalServerError {
			t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusInternalServerError)
		}
	})

	t.Run("сохранение настроек", func(t *testing.T) {
		body := `[{"type":"PAYMENT_SETTLED","channel":"IN_APP","enabled":false}]`
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, authorized(
			httptest.NewRequest(http.MethodPut, "/notify/preferences", strings.NewReader(body)), user, ""))
		if recorder.Code != http.StatusInternalServerError {
			t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusInternalServerError)
		}
	})
}

func TestSetPreferencesValidation(t *testing.T) {
	db := &fakeDatabase{}
	handler := newTestAPI(db, NewHub())
	user := uuid.New()

	tests := []struct {
		name string
		body string
		want int
	}{
		{
			name: "корректные настройки",
			body: `[{"type":"PAYMENT_SETTLED","channel":"IN_APP","enabled":false}]`,
			want: http.StatusNoContent,
		},
		{
			name: "нечитаемое тело",
			body: `[{"type":`,
			want: http.StatusBadRequest,
		},
		{
			name: "неизвестный тип события",
			body: `[{"type":"WHATEVER","channel":"IN_APP","enabled":true}]`,
			want: http.StatusBadRequest,
		},
		{
			name: "неизвестный канал",
			body: `[{"type":"PAYMENT_SETTLED","channel":"SMS","enabled":true}]`,
			want: http.StatusBadRequest,
		},
		{
			// Размер списка задаёт клиент, и без ограничения один запрос
			// превращается в тысячи записей в базу.
			name: "настроек больше, чем существует сочетаний",
			body: tooManyPreferences(),
			want: http.StatusBadRequest,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, authorized(
				httptest.NewRequest(http.MethodPut, "/notify/preferences",
					strings.NewReader(test.body)), user, ""))
			if recorder.Code != test.want {
				t.Errorf("код ответа %d, ожидался %d (%s)", recorder.Code, test.want, recorder.Body)
			}
		})
	}
}

func tooManyPreferences() string {
	count := len(notify.EventTypes())*len(notify.Channels()) + 1
	preferences := make([]notify.Preference, 0, count)
	for range count {
		preferences = append(preferences, notify.Preference{
			Type: notify.EventPaymentSettled, Channel: notify.ChannelInApp, Enabled: true,
		})
	}
	encoded, err := json.Marshal(preferences)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// testMessenger собирает бота поверх подставного API: настоящий Bot API
// в тестах недоступен, а формат запроса задаётся конфигурацией.
func testMessenger(t *testing.T, db Database, handler http.HandlerFunc) *Messenger {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	config := TelegramConfig("bot-token", server.URL, "wish_bot")
	if err := config.Validate(); err != nil {
		t.Fatalf("конфигурация бота: %v", err)
	}
	return NewMessenger(db, config)
}

func TestLinkMessenger(t *testing.T) {
	db := &fakeDatabase{}
	messenger := testMessenger(t, db, func(http.ResponseWriter, *http.Request) {})
	handler := registerHttpHandlers(&api{
		db: db, hub: NewHub(), codeTTL: 15 * time.Minute,
		messengers: map[notify.Channel]*Messenger{notify.ChannelTelegram: messenger},
	})
	user := uuid.New()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, authorized(
		httptest.NewRequest(http.MethodPost, "/notify/messengers/telegram/link", nil), user, ""))

	if recorder.Code != http.StatusOK {
		t.Fatalf("код ответа %d (%s)", recorder.Code, recorder.Body)
	}
	var link struct {
		Provider  notify.Channel `json:"provider"`
		Code      string         `json:"code"`
		Link      string         `json:"link"`
		ExpiresAt time.Time      `json:"expires_at"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &link); err != nil {
		t.Fatalf("разбор ответа: %v", err)
	}
	if link.Provider != notify.ChannelTelegram || link.Code == "" {
		t.Errorf("ответ: %+v", link)
	}
	if !strings.Contains(link.Link, link.Code) {
		t.Errorf("ссылка %q не содержит кода", link.Link)
	}
	if link.ExpiresAt.Before(time.Now()) {
		t.Error("код выдан уже истёкшим")
	}
}

func TestLinkMessengerFailure(t *testing.T) {
	failing := &failingDatabase{err: errors.New("connection refused")}
	messenger := testMessenger(t, failing, func(http.ResponseWriter, *http.Request) {})
	handler := registerHttpHandlers(&api{
		db: failing, hub: NewHub(), codeTTL: 15 * time.Minute,
		messengers: map[notify.Channel]*Messenger{notify.ChannelTelegram: messenger},
	})

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, authorized(
		httptest.NewRequest(http.MethodPost, "/notify/messengers/telegram/link", nil), uuid.New(), ""))
	if recorder.Code != http.StatusInternalServerError {
		t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusInternalServerError)
	}
}

func TestMessengerState(t *testing.T) {
	user := uuid.New()

	t.Run("привязки нет", func(t *testing.T) {
		db := &fakeDatabase{bindingErr: ErrNotFound}
		messenger := testMessenger(t, db, func(http.ResponseWriter, *http.Request) {})
		handler := registerHttpHandlers(&api{
			db: db, hub: NewHub(), codeTTL: time.Minute,
			messengers: map[notify.Channel]*Messenger{notify.ChannelTelegram: messenger},
		})

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, authorized(
			httptest.NewRequest(http.MethodGet, "/notify/messengers/telegram", nil), user, ""))

		if recorder.Code != http.StatusOK {
			t.Fatalf("код ответа %d (%s)", recorder.Code, recorder.Body)
		}
		var state map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &state); err != nil {
			t.Fatalf("разбор ответа: %v", err)
		}
		if state["bound"] != false {
			t.Errorf("состояние %v", state)
		}
	})

	t.Run("привязка есть", func(t *testing.T) {
		db := &fakeDatabase{binding: MessengerBinding{
			ChatId: 4242, Blocked: true, BoundAt: time.Now().UTC(),
		}}
		messenger := testMessenger(t, db, func(http.ResponseWriter, *http.Request) {})
		handler := registerHttpHandlers(&api{
			db: db, hub: NewHub(), codeTTL: time.Minute,
			messengers: map[notify.Channel]*Messenger{notify.ChannelTelegram: messenger},
		})

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, authorized(
			httptest.NewRequest(http.MethodGet, "/notify/messengers/telegram", nil), user, ""))

		if recorder.Code != http.StatusOK {
			t.Fatalf("код ответа %d (%s)", recorder.Code, recorder.Body)
		}
		var state map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &state); err != nil {
			t.Fatalf("разбор ответа: %v", err)
		}
		if state["bound"] != true || state["blocked"] != true {
			t.Errorf("состояние %v", state)
		}
		// Идентификатор чата наружу не отдаётся: пользователю он не нужен,
		// а в ответе это лишние данные о его аккаунте в мессенджере.
		if strings.Contains(recorder.Body.String(), "4242") {
			t.Errorf("идентификатор чата попал в ответ: %s", recorder.Body)
		}
	})

	t.Run("сбой базы", func(t *testing.T) {
		failing := &failingDatabase{err: errors.New("connection refused")}
		messenger := testMessenger(t, failing, func(http.ResponseWriter, *http.Request) {})
		handler := registerHttpHandlers(&api{
			db: failing, hub: NewHub(), codeTTL: time.Minute,
			messengers: map[notify.Channel]*Messenger{notify.ChannelTelegram: messenger},
		})

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, authorized(
			httptest.NewRequest(http.MethodGet, "/notify/messengers/telegram", nil), user, ""))
		if recorder.Code != http.StatusInternalServerError {
			t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusInternalServerError)
		}
	})

	t.Run("канал не настроен", func(t *testing.T) {
		handler := newTestAPI(&fakeDatabase{}, NewHub())
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, authorized(
			httptest.NewRequest(http.MethodGet, "/notify/messengers/telegram", nil), user, ""))
		if recorder.Code != http.StatusNotFound {
			t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusNotFound)
		}
	})
}

// TestUnsubscribeWithoutEmail: канал не настроен — отписываться не от чего.
func TestUnsubscribeWithoutEmail(t *testing.T) {
	handler := newTestAPI(&fakeDatabase{}, NewHub())

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet,
		"/notify/unsubscribe?user="+uuid.NewString()+"&sign=x", nil))
	if recorder.Code != http.StatusNotFound {
		t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusNotFound)
	}
}

func TestUnsubscribeBadUser(t *testing.T) {
	email := newTestEmail(t, Contact{}, &sentMail{})
	handler := registerHttpHandlers(&api{
		db: &fakeDatabase{}, hub: NewHub(), email: email, codeTTL: time.Minute,
	})

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet,
		"/notify/unsubscribe?user=не-uuid&sign=x", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusBadRequest)
	}
}

func TestUnsubscribeStorageFailure(t *testing.T) {
	failing := &failingDatabase{err: errors.New("connection refused")}
	email := newTestEmail(t, Contact{}, &sentMail{})
	handler := registerHttpHandlers(&api{
		db: failing, hub: NewHub(), email: email, codeTTL: time.Minute,
	})

	user := uuid.New()
	link, err := url.Parse(email.UnsubscribeLink(user))
	if err != nil {
		t.Fatalf("разбор ссылки: %v", err)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet,
		"/notify/unsubscribe?"+link.RawQuery, nil))
	if recorder.Code != http.StatusInternalServerError {
		t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusInternalServerError)
	}
}

func TestPublishHandlerFailures(t *testing.T) {
	user := uuid.New()
	body := `{"user_id":"` + user.String() + `","type":"PAYMENT_SETTLED"}`

	t.Run("каналы не читаются", func(t *testing.T) {
		handler := newTestAPI(&channelsFailingDatabase{err: errors.New("connection refused")}, NewHub())
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, authorized(
			httptest.NewRequest(http.MethodPost, "/notify/events", strings.NewReader(body)), user, ""))
		if recorder.Code != http.StatusInternalServerError {
			t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusInternalServerError)
		}
	})

	t.Run("событие не публикуется", func(t *testing.T) {
		db := &fakeDatabase{publishErr: errors.New("connection refused")}
		handler := newTestAPI(db, NewHub())
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, authorized(
			httptest.NewRequest(http.MethodPost, "/notify/events", strings.NewReader(body)), user, ""))
		if recorder.Code != http.StatusInternalServerError {
			t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusInternalServerError)
		}
	})

	t.Run("нечитаемое тело", func(t *testing.T) {
		handler := newTestAPI(&fakeDatabase{}, NewHub())
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, authorized(
			httptest.NewRequest(http.MethodPost, "/notify/events", strings.NewReader(`{"user_id":`)), user, ""))
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusBadRequest)
		}
	})
}

// channelsFailingDatabase отвечает отказом на чтение включённых каналов:
// без списка каналов публиковать событие некуда.
type channelsFailingDatabase struct {
	Database
	err error
}

func (c *channelsFailingDatabase) EnabledChannels(
	context.Context,
	uuid.UUID,
	notify.EventType,
) ([]notify.Channel, error) {
	return nil, c.err
}

// TestSenderChannels фиксирует, за какой канал отвечает каждый отправитель:
// диспетчер выбирает его по этому значению, и ошибка здесь означает
// доставку не туда.
func TestSenderChannels(t *testing.T) {
	inApp := NewInApp(&fakeDatabase{}, &Bus{})
	if inApp.Channel() != notify.ChannelInApp {
		t.Errorf("канал приложения %s", inApp.Channel())
	}

	email := newTestEmail(t, Contact{}, &sentMail{})
	if email.Channel() != notify.ChannelEmail {
		t.Errorf("канал почты %s", email.Channel())
	}

	messenger := testMessenger(t, &fakeDatabase{}, func(http.ResponseWriter, *http.Request) {})
	if messenger.Channel() != notify.ChannelTelegram {
		t.Errorf("канал бота %s", messenger.Channel())
	}
}

// TestBackoffGrowsAndIsCapped: канал, который не отвечает сейчас, чаще
// всего не ответит и через секунду, но расти задержка бесконечно не должна.
//
// Проверяется свойство на всём диапазоне, а не одна точка: произведение
// выходит за диапазон int64, и раньше на amd64 задержка становилась
// отрицательной — задание возвращалось в выборку немедленно.
func TestBackoffGrowsAndIsCapped(t *testing.T) {
	dispatcher := newTestDispatcher(t, &fakeDatabase{})

	previous := time.Duration(0)
	for attempts := range 64 {
		delay := dispatcher.backoff(attempts)
		if delay < 0 {
			t.Fatalf("попытка %d: отрицательная задержка %s", attempts, delay)
		}
		if delay > dispatcher.RetryMax {
			t.Fatalf("попытка %d: задержка %s больше предела %s",
				attempts, delay, dispatcher.RetryMax)
		}
		if delay < previous {
			t.Fatalf("попытка %d: задержка %s меньше предыдущей %s",
				attempts, delay, previous)
		}
		previous = delay
	}
	if previous != dispatcher.RetryMax {
		t.Errorf("задержка не дошла до предела: %s вместо %s", previous, dispatcher.RetryMax)
	}
	if first, second := dispatcher.backoff(0), dispatcher.backoff(1); second <= first {
		t.Errorf("задержка не растёт: %s и %s", first, second)
	}
}

// TestBackoffWithoutCap: без предела задержка обязана упираться в максимум
// Duration, а не заворачиваться через переполнение.
func TestBackoffWithoutCap(t *testing.T) {
	dispatcher := newTestDispatcher(t, &fakeDatabase{})
	dispatcher.RetryMax = 0

	for _, attempts := range []int{0, 30, 63, 1024} {
		if delay := dispatcher.backoff(attempts); delay < 0 {
			t.Errorf("попытка %d: отрицательная задержка %s", attempts, delay)
		}
	}
}

// TestWithinRateDisabled: нулевой предел выключает ограничение частоты,
// и обращения к базе за счётчиком в этом случае быть не должно.
func TestWithinRateDisabled(t *testing.T) {
	dispatcher := newTestDispatcher(t, &fakeDatabase{})
	dispatcher.RateLimit = 0

	allowed, err := dispatcher.withinRate(context.Background(), testTask(0))
	if err != nil {
		t.Fatalf("проверка частоты: %v", err)
	}
	if !allowed {
		t.Error("выключенное ограничение частоты отклонило доставку")
	}
}

// TestMessagesLongPollCancelled: клиент, оборвавший длинный опрос, не должен
// оставлять после себя ни горутины, ни ответа.
func TestMessagesLongPollCancelled(t *testing.T) {
	db := &fakeDatabase{}
	handler := newTestAPI(db, NewHub())
	user := uuid.New()

	ctx, cancel := context.WithCancel(context.Background())
	request := authorized(httptest.NewRequest(http.MethodGet,
		"/notify/messages?wait=20", nil), user, "").WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("обработчик не завершился после обрыва соединения")
	}
}

// flakyMessagesDatabase отдаёт ленту, пока не переключат: так проверяется
// сбой базы на повторном чтении — уже после того, как ожидание разбудили.
type flakyMessagesDatabase struct {
	Database
	mu   sync.Mutex
	fail bool
}

func (f *flakyMessagesDatabase) Messages(context.Context, uuid.UUID, int64, int) ([]notify.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, errors.New("connection refused")
	}
	return nil, nil
}

func (f *flakyMessagesDatabase) breakDown() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = true
}

// TestMessagesFailsOnSecondRead: лента перечитывается из базы после
// пробуждения, потому что порядок задаёт база. Сбой на этом чтении обязан
// стать пятисоткой, а не пустой страницей.
func TestMessagesFailsOnSecondRead(t *testing.T) {
	db := &flakyMessagesDatabase{}
	hub := NewHub()
	handler := registerHttpHandlers(&api{db: db, hub: hub, codeTTL: time.Minute})
	user := uuid.New()

	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(recorder, authorized(
			httptest.NewRequest(http.MethodGet, "/notify/messages?wait=20", nil), user, ""))
	}()

	// Ждём подписку, ломаем базу и будим ожидание.
	deadline := time.Now().Add(5 * time.Second)
	for hub.Subscribers(user) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	db.breakDown()
	hub.Deliver(user, notify.Message{Id: uuid.New(), Seq: 1})

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("длинный опрос не завершился")
	}
	if recorder.Code != http.StatusInternalServerError {
		t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusInternalServerError)
	}
}
