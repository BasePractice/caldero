//go:build integration

package services_test

import (
	"errors"
	"testing"
	"time"

	"wish/services"
	"wish/services/testsupport"

	"github.com/redis/go-redis/v9"
)

func TestRedisCache(t *testing.T) {
	ctx := t.Context()
	cache, err := services.NewDefaultCache(ctx, testsupport.PrepareRedis(t))
	if err != nil {
		t.Fatalf("не удалось подключиться к Redis: %v", err)
	}
	t.Cleanup(func() { _ = cache.Close() })

	t.Run("значение читается тем же, что записано", func(t *testing.T) {
		if err := cache.Set(ctx, "ключ", "значение"); err != nil {
			t.Fatalf("запись: %v", err)
		}
		got, err := cache.Get(ctx, "ключ")
		if err != nil {
			t.Fatalf("чтение: %v", err)
		}
		if got != "значение" {
			t.Errorf("получено %q, ожидалось %q", got, "значение")
		}
	})

	t.Run("значение со сроком жизни исчезает", func(t *testing.T) {
		if err := cache.SetTtl(ctx, "временный", "значение", 50*time.Millisecond); err != nil {
			t.Fatalf("запись: %v", err)
		}
		// Ожидание идёт по факту исчезновения ключа, а не по фиксированной
		// паузе: время в контейнере и в тесте идёт не синхронно.
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := cache.Get(ctx, "временный"); errors.Is(err, redis.Nil) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("значение не исчезло по истечении срока")
	})

	t.Run("отсутствующий ключ отличается от сбоя", func(t *testing.T) {
		if _, err := cache.Get(ctx, "нет-такого"); !errors.Is(err, redis.Nil) {
			t.Errorf("получено %v, ожидалась redis.Nil", err)
		}
	})
}
