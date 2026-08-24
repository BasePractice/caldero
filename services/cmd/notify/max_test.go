package main

import (
	"bytes"
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

// maxStub отвечает вместо Bot API МАКС: обращаться к настоящей площадке
// из тестов нельзя, а разбор её протокола проверить нужно.
//
// Отличия от Telegram здесь не косметические: получатель приходит в строке
// запроса, токен — заголовком, ответ идёт без конверта, а порядковую
// метку задаёт сама площадка.
type maxStub struct {
	mu sync.Mutex

	// updates отдаётся один раз: повторная выдача превратила бы цикл
	// в бесконечную обработку одного и того же обновления.
	updates []map[string]any
	given   bool
	marker  int64

	status   int
	failBody string

	sent    []map[string]any
	queries []string
	auth    []string
	markers []string
	replies []string
}

func (s *maxStub) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()

		s.auth = append(s.auth, r.Header.Get("Authorization"))

		if s.status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(s.status)
			if s.failBody == "" {
				s.failBody = `{"code":"proto.payload","message":"ошибка площадки"}`
			}
			_, _ = w.Write([]byte(s.failBody))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/updates":
			if r.Method != http.MethodGet {
				t.Errorf("обновления запрошены методом %s, ожидался GET", r.Method)
			}
			s.markers = append(s.markers, r.URL.Query().Get("marker"))
			updates := []map[string]any{}
			if !s.given {
				updates = s.updates
				s.given = true
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"updates": updates, "marker": s.marker})
		case "/messages":
			if r.Method != http.MethodPost {
				t.Errorf("сообщение отправлено методом %s, ожидался POST", r.Method)
			}
			s.queries = append(s.queries, r.URL.RawQuery)
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("чтение запроса: %v", err)
				return
			}
			var params map[string]any
			if err = json.Unmarshal(body, &params); err != nil {
				t.Errorf("разбор запроса: %v", err)
				return
			}
			s.sent = append(s.sent, params)
			if text, ok := params["text"].(string); ok {
				s.replies = append(s.replies, text)
			}
			_, _ = w.Write([]byte(`{"message":{"body":{"mid":"mid-1"}}}`))
		default:
			t.Errorf("неизвестный метод площадки: %s", r.URL.Path)
		}
	}
}

func newTestMax(t *testing.T, db Database, stub *maxStub) *Messenger {
	t.Helper()
	server := httptest.NewServer(stub.handler(t))
	t.Cleanup(server.Close)
	return NewMax(db, "max-token", server.URL, "wish_bot")
}

func TestMaxSend(t *testing.T) {
	db := &fakeDatabase{binding: MessengerBinding{UserId: uuid.New(), ChatId: 4242}}
	stub := &maxStub{}
	messenger := newTestMax(t, db, stub)

	if err := messenger.Send(context.Background(), testTask(0), "Заголовок", "Тело"); err != nil {
		t.Fatalf("отправка: %v", err)
	}
	if len(stub.sent) != 1 {
		t.Fatalf("запросов к площадке %d, ожидался 1", len(stub.sent))
	}
	// Получатель идёт параметром строки запроса, а не полем тела:
	// это и есть главное отличие протокола от телеграмного.
	if stub.queries[0] != "chat_id=4242" {
		t.Errorf("строка запроса %q, ожидался chat_id=4242", stub.queries[0])
	}
	// Токен передаётся заголовком без схемы: параметром запроса
	// площадка его больше не принимает.
	if stub.auth[0] != "max-token" {
		t.Errorf("заголовок авторизации %q", stub.auth[0])
	}
	text, _ := stub.sent[0]["text"].(string)
	if !strings.Contains(text, "Заголовок") || !strings.Contains(text, "Тело") {
		t.Errorf("текст сообщения: %q", text)
	}
	if _, ok := stub.sent[0]["format"]; ok {
		// Разметка не включается: в тексте оказываются введённые
		// людьми названия, и любая разметка на них ломается.
		t.Error("сообщение отправлено с разметкой")
	}
}

// TestMaxTruncatesToPlatformLimit: предел площадки — 4000 знаков, а не
// телеграмные 4096, и считать его нужно в знаках, а не в байтах.
func TestMaxTruncatesToPlatformLimit(t *testing.T) {
	db := &fakeDatabase{binding: MessengerBinding{UserId: uuid.New(), ChatId: 1}}
	stub := &maxStub{}
	messenger := newTestMax(t, db, stub)

	long := strings.Repeat("я", 5000)
	if err := messenger.Send(context.Background(), testTask(0), "Заголовок", long); err != nil {
		t.Fatalf("отправка: %v", err)
	}
	text, _ := stub.sent[0]["text"].(string)
	if runes := []rune(text); len(runes) != maxMessageLimit {
		t.Errorf("длина текста %d знаков, ожидалось %d", len(runes), maxMessageLimit)
	}
	if !strings.Contains(text, "я") || strings.Contains(text, "�") {
		t.Error("текст обрезан посреди символа")
	}
}

// TestMaxErrorCodes фиксирует разбор отказов площадки: тело ошибки —
// `{"code", "message"}`, а разряд отказа виден по коду ответа HTTP.
func TestMaxErrorCodes(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		permanent bool
		blocked   bool
	}{
		{
			// Пользователь остановил бота или доступа к чату нет.
			name:      "бота остановили",
			status:    http.StatusForbidden,
			body:      `{"code":"access.denied","message":"bot is stopped"}`,
			permanent: true,
			blocked:   true,
		},
		{
			name:      "чата нет",
			status:    http.StatusNotFound,
			body:      `{"code":"not.found","message":"chat not found"}`,
			permanent: true,
		},
		{
			name:      "неверный запрос",
			status:    http.StatusBadRequest,
			body:      `{"code":"proto.payload","message":"invalid chat_id"}`,
			permanent: true,
		},
		{
			// Ограничение частоты у площадки — 30 запросов в секунду.
			name:   "ограничение частоты",
			status: http.StatusTooManyRequests,
			body:   `{"code":"too.many.requests","message":"slow down"}`,
		},
		{
			name:   "сбой площадки",
			status: http.StatusServiceUnavailable,
			body:   `{"code":"service.unavailable","message":"try later"}`,
		},
		{
			// Неверный токен — ошибка настройки, а не пользователя:
			// отметить его бота заблокированным было бы враньём.
			name:   "неверный токен",
			status: http.StatusUnauthorized,
			body:   `{"code":"verify.token","message":"invalid access_token"}`,
		},
		{
			// Тело отказа может и не разобраться: тогда остаётся
			// код ответа HTTP.
			name:      "отказ без тела",
			status:    http.StatusForbidden,
			body:      "",
			permanent: true,
			blocked:   true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := &fakeDatabase{binding: MessengerBinding{UserId: uuid.New(), ChatId: 1}}
			stub := &maxStub{status: test.status, failBody: test.body}
			messenger := newTestMax(t, db, stub)

			err := messenger.Send(context.Background(), testTask(0), "Заголовок", "Текст")
			if err == nil {
				t.Fatal("отказ площадки принят за успех")
			}
			if Permanent(err) != test.permanent {
				t.Errorf("повторяемость %v, ожидалась %v (%v)", !Permanent(err), !test.permanent, err)
			}
			if errors.Is(err, ErrChannelBlocked) != test.blocked {
				t.Errorf("признак блокировки %v, ожидался %v (%v)",
					errors.Is(err, ErrChannelBlocked), test.blocked, err)
			}
			if db.blocked != test.blocked {
				t.Errorf("отметка блокировки в базе %v, ожидалась %v", db.blocked, test.blocked)
			}
		})
	}
}

// TestMaxBindsByStartPayload: код привязки приходит не сообщением,
// а полем payload события запуска бота. Это единственный способ связать
// чат с пользователем по ссылке.
func TestMaxBindsByStartPayload(t *testing.T) {
	db := &fakeDatabase{completeUser: uuid.New()}
	stub := &maxStub{marker: 77, updates: []map[string]any{{
		"update_type": maxBotStarted,
		"chat_id":     4242,
		"payload":     "ABCD2345",
	}}}
	messenger := newTestMax(t, db, stub)

	runMaxUntilReply(t, messenger, stub)

	if db.completedChat != 4242 {
		t.Errorf("привязан чат %d, ожидался 4242", db.completedChat)
	}
	if !bytes.Equal(db.completedHash, messenger.BindingCodeHash("abcd2345")) {
		t.Error("хеш кода не совпал с ожидаемым")
	}
	if !strings.Contains(stub.lastReply(t), "привязан") {
		t.Errorf("ответ бота: %q", stub.lastReply(t))
	}
}

// TestMaxBindsByMessage: код можно и переписать руками — тогда он приходит
// обычным сообщением, и чат лежит в получателе внутри сообщения.
func TestMaxBindsByMessage(t *testing.T) {
	db := &fakeDatabase{completeUser: uuid.New()}
	stub := &maxStub{updates: []map[string]any{{
		"update_type": maxMessageCreated,
		"message": map[string]any{
			"recipient": map[string]any{"chat_id": 1717},
			"body":      map[string]any{"text": "/start ABCD2345"},
		},
	}}}
	messenger := newTestMax(t, db, stub)

	runMaxUntilReply(t, messenger, stub)

	if db.completedChat != 1717 {
		t.Errorf("привязан чат %d, ожидался 1717", db.completedChat)
	}
}

// TestMaxIgnoresOwnMessages: собственное сообщение бота приходит в том же
// потоке обновлений, и ответ на него превратил бы цикл в переписку бота
// с самим собой.
func TestMaxIgnoresOwnMessages(t *testing.T) {
	db := &fakeDatabase{}
	stub := &maxStub{marker: 3, updates: []map[string]any{{
		"update_type": maxMessageCreated,
		"message": map[string]any{
			"sender":    map[string]any{"is_bot": true},
			"recipient": map[string]any{"chat_id": 4242},
			"body":      map[string]any{"text": "Аккаунт привязан."},
		},
	}}}
	messenger := newTestMax(t, db, stub)

	runMaxUntil(t, messenger, func() bool {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		return len(stub.markers) >= 2
	})

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.replies) != 0 {
		t.Errorf("бот ответил на собственное сообщение: %v", stub.replies)
	}
}

// TestMaxMarksStoppedBot: об остановке бота площадка сообщает событием,
// а не отказом отправки. Отметка обязательна, иначе каждое следующее
// событие упиралось бы в тот же отказ.
func TestMaxMarksStoppedBot(t *testing.T) {
	db := &fakeDatabase{}
	stub := &maxStub{updates: []map[string]any{{
		"update_type": maxBotStopped,
		"chat_id":     4242,
	}}}
	messenger := newTestMax(t, db, stub)

	runMaxUntil(t, messenger, func() bool {
		db.mu.Lock()
		defer db.mu.Unlock()
		return db.blockedChat != 0
	})

	if !db.blocked || db.blockedChat != 4242 {
		t.Errorf("остановка бота не отмечена: blocked=%v chat=%d", db.blocked, db.blockedChat)
	}
	if len(stub.replies) != 0 {
		t.Errorf("бот ответил на остановку: %v", stub.replies)
	}
}

// TestMaxUnblocksOnStart: запущенный заново бот снова получает право
// писать — без этого оповещения в чат не пошли бы уже никогда.
func TestMaxUnblocksOnStart(t *testing.T) {
	db := &fakeDatabase{blocked: true, completeErr: ErrNotFound}
	stub := &maxStub{updates: []map[string]any{{
		"update_type": maxBotStarted,
		"chat_id":     4242,
		"payload":     "ABCD2345",
	}}}
	messenger := newTestMax(t, db, stub)

	runMaxUntilReply(t, messenger, stub)

	if db.blocked {
		t.Error("отметка блокировки не снята при запуске бота")
	}
}

// TestMaxCommitsMarker: метку следующей страницы задаёт площадка, и она
// возвращается в следующем запросе. Без неё обновления повторялись бы
// бесконечно.
func TestMaxCommitsMarker(t *testing.T) {
	db := &fakeDatabase{}
	stub := &maxStub{marker: 512, updates: []map[string]any{{
		"update_type": maxMessageCreated,
		"message": map[string]any{
			"recipient": map[string]any{"chat_id": 9},
			"body":      map[string]any{"text": "привет"},
		},
	}}}
	messenger := newTestMax(t, db, stub)

	runMaxUntilReply(t, messenger, stub)

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.markers) < 2 {
		t.Fatalf("запросов обновлений %d, ожидалось не меньше двух", len(stub.markers))
	}
	// Первый запрос идёт без метки: подтверждать ещё нечего.
	if stub.markers[0] != "" {
		t.Errorf("первая метка %q, ожидалась пустая", stub.markers[0])
	}
	if stub.markers[1] != "512" {
		t.Errorf("вторая метка %q, ожидалась 512", stub.markers[1])
	}
}

// TestMaxSkipsForeignUpdates: в общем потоке приходят и события, до
// которых привязке дела нет. Они пропускаются, но метка всё равно
// подтверждается.
func TestMaxSkipsForeignUpdates(t *testing.T) {
	db := &fakeDatabase{}
	stub := &maxStub{marker: 5, updates: []map[string]any{
		{"update_type": "message_edited", "chat_id": 1},
		{"update_type": "chat_title_changed", "chat_id": 1},
		{"update_type": maxMessageCreated},
	}}
	messenger := newTestMax(t, db, stub)

	runMaxUntil(t, messenger, func() bool {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		return len(stub.markers) >= 2
	})

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.replies) != 0 {
		t.Errorf("бот ответил на чужое событие: %v", stub.replies)
	}
	if stub.markers[1] != "5" {
		t.Errorf("метка не подтверждена: %q", stub.markers[1])
	}
}

func (s *maxStub) lastReply(t *testing.T) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.replies) == 0 {
		t.Fatal("бот ничего не ответил")
	}
	return s.replies[len(s.replies)-1]
}

func runMaxUntilReply(t *testing.T, messenger *Messenger, stub *maxStub) {
	t.Helper()
	runMaxUntil(t, messenger, func() bool {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		return len(stub.replies) > 0
	})
}

func runMaxUntil(t *testing.T, messenger *Messenger, condition func() bool) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- messenger.Run(ctx) }()

	waitFor(t, condition)
	cancel()
	if err := <-done; err != nil {
		t.Errorf("цикл вернул ошибку: %v", err)
	}
}
