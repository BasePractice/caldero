package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
