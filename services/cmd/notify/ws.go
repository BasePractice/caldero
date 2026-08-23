package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"wish/services"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	// wsPingInterval — как часто проверять живость соединения. Без пинга
	// оборванное соединение остаётся в подписчиках до первой записи,
	// а промежуточные прокси закрывают простаивающие соединения молча.
	wsPingInterval = 30 * time.Second
	// wsWriteTimeout ограничивает одну запись в сокет.
	wsWriteTimeout = 10 * time.Second
)

// serveWebSocket отдаёт сообщения активной сессии.
//
// Источником правды остаётся лента: клиент, потерявший соединение,
// дочитывает пропущенное по курсору через длинный опрос, а не ждёт
// повторной рассылки.
func serveWebSocket(hub *Hub, origins []string, w http.ResponseWriter, r *http.Request) {
	authorized, err := services.HttpAuthorized(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Таймауты HTTP-сервера остаются на соединении и после перехвата:
	// с ними сокет закрывался бы через WriteTimeout после подключения,
	// сколько бы ни длилась сессия.
	controller := http.NewResponseController(w)
	if err = errors.Join(
		controller.SetReadDeadline(time.Time{}),
		controller.SetWriteDeadline(time.Time{}),
	); err != nil {
		slog.ErrorContext(r.Context(), "Can't clear connection deadlines",
			slog.String("err", err.Error()))
		http.Error(w, "Can't upgrade connection", http.StatusInternalServerError)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: origins})
	if err != nil {
		// Accept уже ответил клиенту.
		slog.DebugContext(r.Context(), "WebSocket handshake failed", slog.String("err", err.Error()))
		return
	}
	defer func() {
		// Закрытие уже закрытого соединения возвращает ошибку, которая
		// ничего не меняет: обработчик всё равно завершается.
		_ = conn.CloseNow()
	}()

	messages, unsubscribe := hub.Subscribe(authorized.Id)
	defer unsubscribe()

	// CloseRead читает и отбрасывает входящие кадры: клиент нам ничего
	// не сообщает, но без чтения не будет замечено и закрытие соединения.
	ctx := conn.CloseRead(r.Context())

	ticker := time.NewTicker(wsPingInterval)
	defer ticker.Stop()

	slog.DebugContext(ctx, "WebSocket session started", slog.String("user", authorized.Id.String()))
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
			err = conn.Ping(pingCtx)
			cancel()
			if err != nil {
				slog.DebugContext(ctx, "WebSocket ping failed", slog.String("err", err.Error()))
				return
			}
		case message, ok := <-messages:
			if !ok {
				return
			}
			writeCtx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
			err = wsjson.Write(writeCtx, conn, message)
			cancel()
			if err != nil {
				slog.DebugContext(ctx, "Can't write to WebSocket", slog.String("err", err.Error()))
				return
			}
		}
	}
}
