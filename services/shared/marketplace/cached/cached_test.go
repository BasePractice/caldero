package cached

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"wish/services"
	"wish/services/shared/marketplace"
)

// memoryCache — кэш в памяти для тестов. Ручная реализация вместо мока:
// интерфейс из трёх методов, и мок здесь был бы длиннее реализации.
type memoryCache struct {
	mu     sync.Mutex
	values map[string]string
}

func newMemoryCache() *memoryCache {
	return &memoryCache{values: make(map[string]string)}
}

func (c *memoryCache) Get(_ context.Context, key string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	value, ok := c.values[key]
	if !ok {
		return "", errors.New("нет в кэше")
	}
	return value, nil
}

func (c *memoryCache) Set(ctx context.Context, key, value string) error {
	return c.SetTtl(ctx, key, value, 0)
}

func (c *memoryCache) SetTtl(_ context.Context, key, value string, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values[key] = value
	return nil
}

func (c *memoryCache) Close() error { return nil }

// countingCatalog считает обращения к площадке: кэш проверяется тем,
// что второй показ до неё не доходит.
type countingCatalog struct {
	stub  marketplace.Stub
	calls int
}

func (c *countingCatalog) Provider() marketplace.Provider { return marketplace.ProviderStub }

func (c *countingCatalog) Product(ctx context.Context, id string) (marketplace.Product, error) {
	c.calls++
	return c.stub.Product(ctx, id)
}

func (c *countingCatalog) Order(ctx context.Context, id, address string) (string, error) {
	return c.stub.Order(ctx, id, address)
}

func TestCachedAvoidsRepeatedCalls(t *testing.T) {
	catalog := &countingCatalog{}
	cached := New(catalog, newMemoryCache(), time.Minute)

	for range 5 {
		if _, err := cached.Product(context.Background(), "product-1"); err != nil {
			t.Fatalf("карточка: %v", err)
		}
	}
	// У публичных API есть ограничения частоты: дёргать площадку на каждый
	// показ списка желаний нельзя.
	if catalog.calls != 1 {
		t.Errorf("обращений к площадке %d, ожидалось 1", catalog.calls)
	}
}

func TestCachedOpensCircuitOnUnavailable(t *testing.T) {
	catalog := &countingCatalog{stub: marketplace.Stub{Unavailable: true}}
	cached := New(catalog, newMemoryCache(), time.Minute)

	for range 5 {
		if _, err := cached.Product(context.Background(), "product-1"); err == nil {
			t.Fatal("ожидалась ошибка недоступности")
		}
	}
	callsAfterOpen := catalog.calls

	// После размыкания площадку перестают трогать: она восстанавливается
	// тем дольше, чем больше запросов продолжает получать.
	for range 5 {
		if _, err := cached.Product(context.Background(), "product-1"); !errors.Is(err, services.ErrCircuitOpen) {
			t.Fatalf("получено %v, ожидалась ErrCircuitOpen", err)
		}
	}
	if catalog.calls != callsAfterOpen {
		t.Errorf("обращений стало %d, было %d: цепь должна быть разомкнута",
			catalog.calls, callsAfterOpen)
	}
}
