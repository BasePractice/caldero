package main

import (
	"context"
	"testing"
	"time"

	"wish/services"
	"wish/services/shared/notify"

	"github.com/google/uuid"
)

// TestBusWithoutRedis фиксирует локальный режим: без Redis сообщения
// расходятся подписчикам только этого экземпляра, и это рабочая
// конфигурация, а не отказ.
func TestBusWithoutRedis(t *testing.T) {
	hub := NewHub()
	bus, err := NewBus(t.Context(), services.Config{}, hub)
	if err != nil {
		t.Fatalf("создание шины: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })

	user := uuid.New()
	messages, unsubscribe := hub.Subscribe(user)
	defer unsubscribe()

	sent := notify.Message{Id: uuid.New(), Seq: 1, Title: "Подарок выбран"}
	if err := bus.Publish(t.Context(), user, sent); err != nil {
		t.Fatalf("публикация: %v", err)
	}

	select {
	case received := <-messages:
		if received.Id != sent.Id {
			t.Errorf("получено %+v, ожидалось %+v", received, sent)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("сообщение не дошло до локального подписчика")
	}
}

// TestBusRunStopsOnContext: в локальном режиме Run ждёт отмены контекста —
// горутина без пути выхода это утечка, а не стилистика.
func TestBusRunStopsOnContext(t *testing.T) {
	bus, err := NewBus(t.Context(), services.Config{}, NewHub())
	if err != nil {
		t.Fatalf("создание шины: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- bus.Run(ctx) }()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("остановка вернула ошибку: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("шина не остановилась по отмене контекста")
	}
}

func TestBusBadRedisURL(t *testing.T) {
	if _, err := NewBus(t.Context(), services.Config{RedisURL: "не-адрес"}, NewHub()); err == nil {
		t.Fatal("некорректный адрес принят")
	}
}
