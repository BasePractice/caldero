package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// telegramStub отвечает вместо Bot API: обращаться к настоящему Telegram
// из тестов нельзя, а поведение канала проверить нужно.
type telegramStub struct {
	status   int
	response string
	requests []map[string]any
}

func (s *telegramStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var params map[string]any
		body := new(bytes.Buffer)
		if _, err := body.ReadFrom(r.Body); err == nil {
			_ = json.Unmarshal(body.Bytes(), &params)
		}
		s.requests = append(s.requests, params)

		w.Header().Set("Content-Type", "application/json")
		if s.status != 0 {
			w.WriteHeader(s.status)
		}
		if s.response == "" {
			s.response = `{"ok":true,"result":{}}`
		}
		_, _ = w.Write([]byte(s.response))
	}
}

func newTestTelegram(t *testing.T, db Database, stub *telegramStub) *Messenger {
	t.Helper()
	server := httptest.NewServer(stub.handler())
	t.Cleanup(server.Close)
	return NewTelegram(db, "test-token", server.URL, "wish_bot")
}

func TestTelegramSend(t *testing.T) {
	db := &fakeDatabase{binding: MessengerBinding{UserId: uuid.New(), ChatId: 4242}}
	stub := &telegramStub{}
	telegram := newTestTelegram(t, db, stub)

	if err := telegram.Send(context.Background(), testTask(0), "Заголовок", "Тело"); err != nil {
		t.Fatalf("отправка: %v", err)
	}
	if len(stub.requests) != 1 {
		t.Fatalf("запросов к Bot API %d, ожидался 1", len(stub.requests))
	}
	if chat, ok := stub.requests[0]["chat_id"].(float64); !ok || int64(chat) != 4242 {
		t.Errorf("chat_id = %v, ожидался 4242", stub.requests[0]["chat_id"])
	}
	text, _ := stub.requests[0]["text"].(string)
	if !strings.Contains(text, "Заголовок") || !strings.Contains(text, "Тело") {
		t.Errorf("текст сообщения: %q", text)
	}
	// Разметка не включается: в тексте оказываются введённые людьми
	// названия, и любая разметка на них ломается.
	if _, ok := stub.requests[0]["parse_mode"]; ok {
		t.Error("сообщение отправлено с разметкой")
	}
}

func TestTelegramBlocked(t *testing.T) {
	db := &fakeDatabase{binding: MessengerBinding{UserId: uuid.New(), ChatId: 1}}
	stub := &telegramStub{
		status:   http.StatusForbidden,
		response: `{"ok":false,"error_code":403,"description":"bot was blocked by the user"}`,
	}
	telegram := newTestTelegram(t, db, stub)

	err := telegram.Send(context.Background(), testTask(0), "Заголовок", "Тело")
	if !errors.Is(err, ErrChannelBlocked) {
		t.Fatalf("получено %v, ожидалась %v", err, ErrChannelBlocked)
	}
	// Отметка обязательна: без неё каждое следующее событие заново
	// упиралось бы в тот же отказ.
	if !db.blocked {
		t.Error("блокировка бота не отмечена")
	}
}

func TestTelegramUnavailable(t *testing.T) {
	db := &fakeDatabase{binding: MessengerBinding{UserId: uuid.New(), ChatId: 1}}
	stub := &telegramStub{
		status:   http.StatusTooManyRequests,
		response: `{"ok":false,"error_code":429,"description":"Too Many Requests"}`,
	}
	telegram := newTestTelegram(t, db, stub)

	err := telegram.Send(context.Background(), testTask(0), "Заголовок", "Тело")
	if !errors.Is(err, ErrChannelUnavailable) {
		t.Errorf("получено %v, ожидалась %v", err, ErrChannelUnavailable)
	}
	if db.blocked {
		t.Error("ограничение частоты принято за блокировку")
	}
}

func TestTelegramUnbound(t *testing.T) {
	db := &fakeDatabase{bindingErr: ErrNotFound}
	stub := &telegramStub{}
	telegram := newTestTelegram(t, db, stub)

	err := telegram.Send(context.Background(), testTask(0), "Заголовок", "Тело")
	if !errors.Is(err, ErrChannelUnbound) {
		t.Errorf("получено %v, ожидалась %v", err, ErrChannelUnbound)
	}
	if len(stub.requests) != 0 {
		t.Error("запрос к Bot API при отсутствующей привязке")
	}
}

func TestTelegramBinding(t *testing.T) {
	user := uuid.New()
	db := &fakeDatabase{completeUser: user}
	stub := &telegramStub{}
	telegram := newTestTelegram(t, db, stub)

	telegram.handleCommand(context.Background(), 777, "/start ABCD2345")

	if db.completedChat != 777 {
		t.Errorf("привязан чат %d, ожидался 777", db.completedChat)
	}
	if !bytes.Equal(db.completedHash, telegram.BindingCodeHash("abcd2345")) {
		// Регистр не должен иметь значения: код переписывают руками.
		t.Error("хеш кода не совпал с ожидаемым")
	}
	if len(stub.requests) != 1 {
		t.Fatalf("ответов пользователю %d, ожидался 1", len(stub.requests))
	}
}

func TestTelegramBindingWrongCode(t *testing.T) {
	db := &fakeDatabase{completeErr: ErrNotFound}
	stub := &telegramStub{}
	telegram := newTestTelegram(t, db, stub)

	telegram.handleCommand(context.Background(), 1, "/start WRONGCOD")

	if len(stub.requests) != 1 {
		t.Fatalf("ответов пользователю %d, ожидался 1", len(stub.requests))
	}
	text, _ := stub.requests[0]["text"].(string)
	// Ответ не должен различать «нет такого кода» и «код просрочен»:
	// по разнице ответов подбирать код удобнее.
	if strings.Contains(strings.ToLower(text), "просроч") {
		t.Errorf("ответ раскрывает причину отказа: %q", text)
	}
}

func TestBindingCodeAlphabet(t *testing.T) {
	code, err := NewBindingCode(func(buffer []byte) (int, error) {
		for i := range buffer {
			buffer[i] = byte(i)
		}
		return len(buffer), nil
	})
	if err != nil {
		t.Fatalf("генерация кода: %v", err)
	}
	if len(code) != 8 {
		t.Errorf("длина кода %d, ожидалось 8", len(code))
	}
	// Похожие знаки исключены: код переписывают руками из мессенджера.
	if strings.ContainsAny(code, "OI01") {
		t.Errorf("код содержит неразличимые знаки: %q", code)
	}
}

func TestBindingLink(t *testing.T) {
	db := &fakeDatabase{}

	// Вид ссылки у площадок разный: Telegram передаёт код параметром
	// запроса, МАКС — частью пути.
	if link := NewTelegram(db, "t", "", "wish_bot").BindingLink("ABCD2345"); link !=
		"https://t.me/wish_bot?start=ABCD2345" {
		t.Errorf("ссылка привязки Telegram: %q", link)
	}
	if link := NewMax(db, "t", "", "wish_bot").BindingLink("ABCD2345"); link !=
		"https://max.ru/wish_bot/start/ABCD2345" {
		t.Errorf("ссылка привязки МАКС: %q", link)
	}

	// Без имени бота ссылку не собрать: остаётся код, который вводят
	// руками.
	if link := NewTelegram(db, "t", "", "").BindingLink("ABCD2345"); link != "" {
		t.Errorf("ссылка без имени бота: %q", link)
	}
	if link := NewMax(db, "t", "", "").BindingLink("ABCD2345"); link != "" {
		t.Errorf("ссылка без имени бота: %q", link)
	}
}
