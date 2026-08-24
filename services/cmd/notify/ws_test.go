package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wish/services/shared/notify"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"
)

// TestWebSocketDelivers проверяет мгновенную доставку: сообщение, попавшее
// в ленту, уходит в открытое соединение без ожидания опроса.
func TestWebSocketDelivers(t *testing.T) {
	hub := NewHub()
	user := uuid.New()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveWebSocket(hub, []string{"*"}, w, r)
	}))
	defer server.Close()

	conn, response, err := websocket.Dial(t.Context(),
		"ws"+strings.TrimPrefix(server.URL, "http")+"/notify/ws",
		&websocket.DialOptions{
			HTTPHeader: http.Header{"X-Authorized-Id": {user.String()}},
		})
	if err != nil {
		t.Fatalf("подключение: %v", err)
	}
	if response.Body != nil {
		_ = response.Body.Close()
	}
	defer func() { _ = conn.CloseNow() }()

	// Подписка появляется не мгновенно: обработчик успевает выполниться
	// уже после ответа на рукопожатие.
	sent := notify.Message{
		Id: uuid.New(), Seq: 1, Type: notify.EventWishlistItemReserved,
		Title: "Подарок выбран", Body: "Кофеварка", CreatedAt: time.Now().UTC(),
	}
	deadline := time.Now().Add(5 * time.Second)
	for hub.Subscribers(user) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	hub.Deliver(user, sent)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	var received notify.Message
	if err := wsjson.Read(ctx, conn, &received); err != nil {
		t.Fatalf("чтение сообщения: %v", err)
	}
	if received.Id != sent.Id || received.Title != sent.Title {
		t.Errorf("получено %+v, ожидалось %+v", received, sent)
	}
}

// TestWebSocketUnauthorized: сокет открывается только проверенному
// пользователю — иначе чужая лента читается без токена.
func TestWebSocketUnauthorized(t *testing.T) {
	recorder := httptest.NewRecorder()
	serveWebSocket(NewHub(), nil, recorder,
		httptest.NewRequest(http.MethodGet, "/notify/ws", nil))

	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusUnauthorized)
	}
}

// TestWebSocketWrongOrigin: список источников задаётся конфигурацией,
// и чужая страница не должна открывать сокет от имени пользователя.
func TestWebSocketWrongOrigin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveWebSocket(NewHub(), []string{"wish.example"}, w, r)
	}))
	defer server.Close()

	conn, response, err := websocket.Dial(t.Context(),
		"ws"+strings.TrimPrefix(server.URL, "http")+"/notify/ws",
		&websocket.DialOptions{
			HTTPHeader: http.Header{
				"X-Authorized-Id": {uuid.NewString()},
				"Origin":          {"https://attacker.example"},
			},
		})
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err == nil {
		_ = conn.CloseNow()
		t.Fatal("соединение с чужого источника принято")
	}
}

// TestWebSocketWithoutHijack: обычный httptest.ResponseRecorder перехват
// соединения не поддерживает — рукопожатие обязано закончиться отказом,
// а не паникой обработчика.
func TestWebSocketWithoutHijack(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/notify/ws", nil)
	request.Header.Set("X-Authorized-Id", uuid.NewString())

	serveWebSocket(NewHub(), nil, recorder, request)
	if recorder.Code == http.StatusSwitchingProtocols {
		t.Error("рукопожатие прошло без поддержки перехвата соединения")
	}
}

// TestWebSocketStopsWhenSubscriptionCloses: подписка закрывается вместе
// с остановкой концентратора, и сессия обязана завершиться, а не остаться
// висеть с открытым сокетом.
func TestWebSocketStopsWhenSubscriptionCloses(t *testing.T) {
	hub := NewHub()
	user := uuid.New()

	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveWebSocket(hub, []string{"*"}, w, r)
		close(done)
	}))
	defer server.Close()

	conn, response, err := websocket.Dial(t.Context(),
		"ws"+strings.TrimPrefix(server.URL, "http")+"/notify/ws",
		&websocket.DialOptions{HTTPHeader: http.Header{"X-Authorized-Id": {user.String()}}})
	if err != nil {
		t.Fatalf("подключение: %v", err)
	}
	if response.Body != nil {
		_ = response.Body.Close()
	}

	deadline := time.Now().Add(5 * time.Second)
	for hub.Subscribers(user) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	// Клиент уходит: обработчик обязан заметить это и завершиться.
	_ = conn.CloseNow()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("сессия не завершилась после ухода клиента")
	}

	if hub.Subscribers(user) != 0 {
		t.Error("подписка осталась после завершения сессии")
	}
}
