package services

import (
	"testing"
)

// TestNewRedisCacheBadURL: адрес приходит из конфигурации, и разбор обязан
// падать на старте, а не при первом обращении к кэшу.
func TestNewRedisCacheBadURL(t *testing.T) {
	if _, err := NewRedisCache(t.Context(), Config{RedisURL: "не-адрес"}); err == nil {
		t.Fatal("некорректный адрес принят")
	}
}

// TestNewRedisCacheUnreachable: недоступный Redis обнаруживается пингом
// при создании, иначе сервис поднимается и падает на первом запросе.
func TestNewRedisCacheUnreachable(t *testing.T) {
	addr := freePort(t)

	if _, err := NewDefaultCache(t.Context(), Config{RedisURL: "redis://" + addr + "/0"}); err == nil {
		t.Fatal("недоступный Redis принят")
	}
}
