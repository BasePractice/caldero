package marketplace

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"wish/services"
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

// countingCatalog считает обращения к площадке.
type countingCatalog struct {
	stub  Stub
	calls int
}

func (c *countingCatalog) Provider() Provider { return ProviderStub }

func (c *countingCatalog) Product(ctx context.Context, id string) (Product, error) {
	c.calls++
	return c.stub.Product(ctx, id)
}

func (c *countingCatalog) Order(ctx context.Context, id, address string) (string, error) {
	c.calls++
	return c.stub.Order(ctx, id, address)
}

func TestStubIsDeterministic(t *testing.T) {
	stub := &Stub{}
	first, err := stub.Product(context.Background(), "product-1")
	if err != nil {
		t.Fatalf("карточка: %v", err)
	}
	second, err := stub.Product(context.Background(), "product-1")
	if err != nil {
		t.Fatalf("карточка: %v", err)
	}
	// Иначе тесты, опирающиеся на цену, становятся невоспроизводимыми.
	if first.Price != second.Price {
		t.Errorf("цена изменилась между вызовами: %s и %s", first.Price, second.Price)
	}
	if first.Price <= 0 {
		t.Errorf("цена должна быть положительной, получено %s", first.Price)
	}
}

func TestCachedAvoidsRepeatedCalls(t *testing.T) {
	catalog := &countingCatalog{}
	cached := NewCached(catalog, newMemoryCache(), time.Minute)

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
	catalog := &countingCatalog{stub: Stub{Unavailable: true}}
	cached := NewCached(catalog, newMemoryCache(), time.Minute)

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

func TestStubOrderIsUnsupportedByDefault(t *testing.T) {
	stub := &Stub{}
	// Публичные API площадок ориентированы на продавца, и оформление заказа
	// от имени покупателя доступно не везде — поведение по умолчанию
	// отражает именно это.
	if _, err := stub.Order(context.Background(), "product-1", "адрес"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("получено %v, ожидалась ErrUnsupported", err)
	}
}

func TestRegistryResolvesProvider(t *testing.T) {
	registry := NewRegistry(&Stub{})

	if _, err := registry.Catalog(ProviderStub); err != nil {
		t.Errorf("заглушка не найдена: %v", err)
	}
	if _, err := registry.Catalog(ProviderOzon); err == nil {
		t.Error("неподключённая площадка не должна находиться")
	}
	if providers := registry.Providers(); len(providers) != 1 {
		t.Errorf("площадок %d, ожидалась 1", len(providers))
	}
}
