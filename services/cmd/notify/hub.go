package main

import (
	"sync"

	"wish/services/shared/notify"

	"github.com/google/uuid"
)

// subscriberBuffer — сколько сообщений держится для подписчика, пока он
// их не забрал. Буфер небольшой: отставший клиент дочитает ленту из базы
// по курсору, и копить для него сообщения в памяти незачем.
const subscriberBuffer = 8

// Hub раздаёт сообщения подписчикам этого экземпляра сервиса: активным
// WebSocket-соединениям и ожидающим запросам длинного опроса.
//
// Хаб — только доставка «прямо сейчас». Источником правды остаётся лента
// в базе: подписчик, который что-то пропустил, забирает пропущенное
// по курсору, а не переспрашивает хаб.
type Hub struct {
	mu          sync.Mutex
	next        uint64
	subscribers map[uuid.UUID]map[uint64]chan notify.Message
}

func NewHub() *Hub {
	return &Hub{subscribers: make(map[uuid.UUID]map[uint64]chan notify.Message)}
}

// Subscribe возвращает канал сообщений пользователя и функцию отписки.
// Отписка обязательна: без неё канал остаётся в карте навсегда.
func (h *Hub) Subscribe(user uuid.UUID) (<-chan notify.Message, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()

	id := h.next
	h.next++
	messages := make(chan notify.Message, subscriberBuffer)
	if _, ok := h.subscribers[user]; !ok {
		h.subscribers[user] = make(map[uint64]chan notify.Message)
	}
	h.subscribers[user][id] = messages

	return messages, func() {
		h.mu.Lock()
		defer h.mu.Unlock()

		subscribers, ok := h.subscribers[user]
		if !ok {
			return
		}
		if channel, ok := subscribers[id]; ok {
			delete(subscribers, id)
			close(channel)
		}
		if len(subscribers) == 0 {
			delete(h.subscribers, user)
		}
	}
}

// Deliver рассылает сообщение подписчикам пользователя.
//
// Отправка неблокирующая: подписчик, который не успевает читать, не должен
// останавливать доставку остальным. Пропущенное он получит из ленты.
func (h *Hub) Deliver(user uuid.UUID, message notify.Message) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, subscriber := range h.subscribers[user] {
		select {
		case subscriber <- message:
		default:
		}
	}
}

// Subscribers сообщает число подписчиков пользователя на этом экземпляре.
func (h *Hub) Subscribers(user uuid.UUID) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subscribers[user])
}
