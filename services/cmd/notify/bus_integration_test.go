//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"wish/services/shared/notify"
	"wish/services/testsupport"

	"github.com/google/uuid"
)

// TestBusAcrossInstances проверяет то, ради чего шина существует: доставку
// выполняет один экземпляр, а соединение пользователя держит другой,
// и без шины WebSocket работал бы только там, где сообщение создано.
func TestBusAcrossInstances(t *testing.T) {
	cfg := testsupport.PrepareRedis(t)

	publisherHub := NewHub()
	publisher, err := NewBus(t.Context(), cfg, publisherHub)
	if err != nil {
		t.Fatalf("создание шины отправителя: %v", err)
	}
	t.Cleanup(func() { _ = publisher.Close() })

	subscriberHub := NewHub()
	subscriber, err := NewBus(t.Context(), cfg, subscriberHub)
	if err != nil {
		t.Fatalf("создание шины подписчика: %v", err)
	}
	t.Cleanup(func() { _ = subscriber.Close() })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- subscriber.Run(ctx) }()

	user := uuid.New()
	messages, unsubscribe := subscriberHub.Subscribe(user)
	defer unsubscribe()

	sent := notify.Message{
		Id: uuid.New(), Seq: 7, Type: notify.EventWishlistItemGifted,
		Title: "Подарок вручён", Body: "Кофеварка", CreatedAt: time.Now().UTC(),
	}

	// Подписка Redis устанавливается асинхронно, поэтому публикация
	// повторяется до доставки: одна попытка могла бы её опередить.
	deadline := time.Now().Add(20 * time.Second)
	for {
		if err := publisher.Publish(ctx, user, sent); err != nil {
			t.Fatalf("публикация: %v", err)
		}
		select {
		case received := <-messages:
			if received.Id != sent.Id || received.Title != sent.Title {
				t.Errorf("получено %+v, ожидалось %+v", received, sent)
			}
			cancel()
			if err := <-done; err != nil {
				t.Errorf("остановка шины: %v", err)
			}
			return
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("сообщение не дошло до подписчика другого экземпляра")
		}
	}
}

// TestBusUnreachableRedis: недоступный Redis обнаруживается при создании,
// а не на первой публикации.
func TestBusUnreachableRedis(t *testing.T) {
	cfg := testsupport.PrepareRedis(t)
	// Адрес заведомо свободного порта: контейнер поднят, но шина
	// направляется мимо него.
	cfg.RedisURL = "redis://127.0.0.1:1/0"

	if _, err := NewBus(t.Context(), cfg, NewHub()); err == nil {
		t.Fatal("недоступный Redis принят")
	}
}
