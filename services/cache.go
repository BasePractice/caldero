package services

import (
	"context"
	"time"
)

// Cache интерфейс для работы с кешом
type Cache interface {
	// Get Получения значения из кеша по ключу
	Get(ctx context.Context, key string) (string, error)
	// SetTtl установка значения по ключу с временем жизни expiration
	SetTtl(ctx context.Context, key string, value string, expiration time.Duration) error
	// Set установка значения по ключу
	Set(ctx context.Context, key string, value string) error
}
