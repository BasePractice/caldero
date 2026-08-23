package main

import (
	"testing"
	"time"

	"wish/services/shared/notify"

	"github.com/google/uuid"
)

func TestHubDelivery(t *testing.T) {
	hub := NewHub()
	user := uuid.New()

	first, unsubscribeFirst := hub.Subscribe(user)
	second, unsubscribeSecond := hub.Subscribe(user)
	defer unsubscribeSecond()

	message := notify.Message{Seq: 1, Title: "Подарок"}
	hub.Deliver(user, message)

	for name, subscriber := range map[string]<-chan notify.Message{"первый": first, "второй": second} {
		select {
		case received := <-subscriber:
			if received.Seq != message.Seq {
				t.Errorf("%s подписчик получил seq %d, ожидался %d", name, received.Seq, message.Seq)
			}
		case <-time.After(time.Second):
			t.Errorf("%s подписчик не получил сообщение", name)
		}
	}

	t.Run("отписка закрывает канал", func(t *testing.T) {
		unsubscribeFirst()
		select {
		case _, ok := <-first:
			if ok {
				t.Error("канал отписавшегося подписчика не закрыт")
			}
		case <-time.After(time.Second):
			t.Error("канал отписавшегося подписчика не закрыт")
		}
		if got := hub.Subscribers(user); got != 1 {
			t.Errorf("подписчиков %d, ожидался 1", got)
		}
	})

	t.Run("чужие сообщения не доставляются", func(t *testing.T) {
		hub.Deliver(uuid.New(), notify.Message{Seq: 2})
		select {
		case received := <-second:
			t.Errorf("получено чужое сообщение: %+v", received)
		case <-time.After(100 * time.Millisecond):
		}
	})
}

// TestHubDoesNotBlock проверяет, что отставший подписчик не останавливает
// доставку: пропущенное он заберёт из ленты по курсору.
func TestHubDoesNotBlock(t *testing.T) {
	hub := NewHub()
	user := uuid.New()
	_, unsubscribe := hub.Subscribe(user)
	defer unsubscribe()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range subscriberBuffer * 4 {
			hub.Deliver(user, notify.Message{Seq: int64(i)})
		}
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("доставка заблокировалась на неуспевающем подписчике")
	}
}
