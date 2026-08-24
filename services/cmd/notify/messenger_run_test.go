package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// botAPI изображает Bot API: отдаёт заранее заданные обновления и запоминает
// отправленные сообщения. Настоящего бота в тестах нет, а формат запроса
// задаётся конфигурацией — проверять здесь нужно поведение цикла.
type botAPI struct {
	mu sync.Mutex
	// updates отдаётся один раз: повторная выдача превратила бы цикл
	// в бесконечную обработку одного и того же обновления.
	updates []map[string]any
	given   bool
	replies []string
	failNow bool
}

func (b *botAPI) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()

		if b.failNow {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("чтение запроса бота: %v", err)
			return
		}
		var params map[string]any
		if err := json.Unmarshal(body, &params); err != nil {
			t.Errorf("разбор запроса бота: %v", err)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			result := []map[string]any{}
			if !b.given {
				result = b.updates
				b.given = true
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			if text, ok := params["text"].(string); ok {
				b.replies = append(b.replies, text)
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
		default:
			t.Errorf("неизвестный метод бота: %s", r.URL.Path)
		}
	}
}

func (b *botAPI) reply(t *testing.T) string {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.replies) == 0 {
		t.Fatal("бот ничего не ответил")
	}
	return b.replies[len(b.replies)-1]
}

func update(id int64, chat int64, text string) map[string]any {
	return map[string]any{
		"update_id": id,
		"message": map[string]any{
			"chat": map[string]any{"id": chat},
			"text": text,
		},
	}
}

// TestMessengerBindsByCommand: привязка происходит по команде /start
// с кодом — это единственный способ связать чат с пользователем.
func TestMessengerBindsByCommand(t *testing.T) {
	user := uuid.New()
	db := &fakeDatabase{completeUser: user}
	bot := &botAPI{updates: []map[string]any{update(1, 4242, "/start ABCD12")}}
	messenger := testMessenger(t, db, bot.handler(t))

	runUntilQuiet(t, messenger, bot)

	if db.completedChat != 4242 {
		t.Errorf("привязан чат %d, ожидался 4242", db.completedChat)
	}
	// В базу уходит хеш кода, а не сам код: список действующих кодов —
	// это список готовых способов привязать чужой аккаунт.
	if len(db.completedHash) == 0 {
		t.Error("код привязки не захеширован")
	}
	if !strings.Contains(bot.reply(t), "привязан") {
		t.Errorf("ответ бота: %q", bot.reply(t))
	}
}

func TestMessengerCommandErrors(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		db      *fakeDatabase
		wantRep string
	}{
		{
			name:    "команда без кода",
			text:    "/start",
			db:      &fakeDatabase{},
			wantRep: "код привязки",
		},
		{
			name:    "не команда",
			text:    "привет",
			db:      &fakeDatabase{},
			wantRep: "/start",
		},
		{
			// Причина не уточняется: по разнице ответов «не найден»
			// и «просрочен» подбирать код удобнее.
			name:    "неизвестный код",
			text:    "/start ABCD12",
			db:      &fakeDatabase{completeErr: ErrNotFound},
			wantRep: "не подошёл",
		},
		{
			name:    "сбой базы",
			text:    "/start ABCD12",
			db:      &fakeDatabase{completeErr: errors.New("connection refused")},
			wantRep: "Попробуйте позже",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bot := &botAPI{updates: []map[string]any{update(1, 4242, test.text)}}
			messenger := testMessenger(t, test.db, bot.handler(t))

			runUntilQuiet(t, messenger, bot)
			if !strings.Contains(bot.reply(t), test.wantRep) {
				t.Errorf("ответ бота %q, ожидалось упоминание %q", bot.reply(t), test.wantRep)
			}
		})
	}
}

// TestMessengerSkipsNonMessages: обновление без сообщения пропускается,
// но подтверждается — иначе Bot API повторяет его бесконечно.
func TestMessengerSkipsNonMessages(t *testing.T) {
	db := &fakeDatabase{}
	bot := &botAPI{updates: []map[string]any{{"update_id": 1}}}
	messenger := testMessenger(t, db, bot.handler(t))

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- messenger.Run(ctx) }()

	waitFor(t, func() bool {
		bot.mu.Lock()
		defer bot.mu.Unlock()
		return bot.given
	})
	cancel()
	if err := <-done; err != nil {
		t.Errorf("цикл вернул ошибку: %v", err)
	}

	bot.mu.Lock()
	defer bot.mu.Unlock()
	if len(bot.replies) != 0 {
		t.Errorf("бот ответил на обновление без сообщения: %v", bot.replies)
	}
}

// TestMessengerSurvivesAPIFailure: недоступный Bot API не должен ни ронять
// цикл, ни превращать его в непрерывный поток запросов.
func TestMessengerSurvivesAPIFailure(t *testing.T) {
	bot := &botAPI{failNow: true}
	messenger := testMessenger(t, &fakeDatabase{}, bot.handler(t))

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()

	if err := messenger.Run(ctx); err != nil {
		t.Errorf("цикл вернул ошибку вместо остановки по контексту: %v", err)
	}
}

// TestMessengerStopsOnCancelledContext: отменённый контекст останавливает
// цикл сразу, не делая ни одного запроса.
func TestMessengerStopsOnCancelledContext(t *testing.T) {
	called := false
	messenger := testMessenger(t, &fakeDatabase{}, func(http.ResponseWriter, *http.Request) {
		called = true
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := messenger.Run(ctx); err != nil {
		t.Errorf("цикл вернул ошибку: %v", err)
	}
	if called {
		t.Error("сделан запрос к боту при отменённом контексте")
	}
}

// runUntilQuiet прогоняет цикл бота, пока он не ответит на обновление.
func runUntilQuiet(t *testing.T, messenger *Messenger, bot *botAPI) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- messenger.Run(ctx) }()

	waitFor(t, func() bool {
		bot.mu.Lock()
		defer bot.mu.Unlock()
		return len(bot.replies) > 0
	})
	cancel()
	if err := <-done; err != nil {
		t.Errorf("цикл вернул ошибку: %v", err)
	}
}

// waitFor ждёт выполнения условия, а не фиксированную паузу: цикл бота
// работает в своей горутине, и время его прохода заранее неизвестно.
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("условие не выполнилось за отведённое время")
}

// TestMessengerServerErrorIsNotFatal проверяет разбор отказа самого Bot API:
// он отвечает 200 с ok=false, и такой ответ обязан стать ошибкой.
func TestMessengerAPIError(t *testing.T) {
	messenger := testMessenger(t, &fakeDatabase{}, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error_code":403,"description":"bot was blocked by the user"}`))
	})

	_, _, err := messenger.dialect.Updates(t.Context(), 0)
	if err == nil {
		t.Fatal("отказ Bot API принят за успех")
	}
	var apiErr *telegramError
	if !errors.As(err, &apiErr) || apiErr.Code != 403 {
		t.Errorf("ошибка %v, ожидался разобранный отказ Bot API", err)
	}
}

func TestMessengerTransportError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	messenger := NewTelegram(&fakeDatabase{}, "bot-token", server.URL, "wish_bot")
	server.Close()

	if _, _, err := messenger.dialect.Updates(t.Context(), 0); err == nil {
		t.Fatal("недоступный Bot API принят за успех")
	}
}

// TestMessengerErrorCodes фиксирует разбор отказов Bot API: по коду видно,
// повторять доставку или бросить. Блокировку бота повторять бессмысленно,
// а ограничение частоты — наоборот.
func TestMessengerErrorCodes(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		status    int
		permanent bool
	}{
		{
			// Пользователь заблокировал бота: сообщений больше не будет,
			// сколько ни повторяй.
			name:      "бот заблокирован",
			body:      `{"ok":false,"error_code":403,"description":"bot was blocked by the user"}`,
			permanent: true,
		},
		{
			// Чат не найден или удалён: повторять нечего.
			name:      "чат не найден",
			body:      `{"ok":false,"error_code":400,"description":"chat not found"}`,
			permanent: true,
		},
		{
			name: "ограничение частоты",
			body: `{"ok":false,"error_code":429,"description":"too many requests"}`,
		},
		{
			name: "сбой самого Bot API",
			body: `{"ok":false,"error_code":500,"description":"internal server error"}`,
		},
		{
			// Код в теле не пришёл: берётся код ответа HTTP.
			name:      "код только в ответе HTTP",
			body:      `{"ok":false,"description":"forbidden"}`,
			status:    http.StatusForbidden,
			permanent: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			messenger := testMessenger(t, &fakeDatabase{}, func(w http.ResponseWriter, _ *http.Request) {
				if test.status != 0 {
					w.WriteHeader(test.status)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.body))
			})

			err := messenger.Send(t.Context(), testTask(0), "Заголовок", "Текст")
			if err == nil {
				t.Fatal("отказ Bot API принят за успех")
			}
			if Permanent(err) != test.permanent {
				t.Errorf("повторяемость %v, ожидалась %v (%v)", !Permanent(err), !test.permanent, err)
			}
		})
	}
}
