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

func TestProvider(t *testing.T) {
	catalog := &countingCatalog{}
	cached := New(catalog, newMemoryCache(), time.Minute)

	if cached.Provider() != catalog.Provider() {
		t.Errorf("площадка %s, ожидалась %s", cached.Provider(), catalog.Provider())
	}
}

// TestOrderGoesThroughBreaker: заказ не кэшируется — кэшировать результат
// покупки нельзя, — но размыкатель на него распространяется.
func TestOrderGoesThroughBreaker(t *testing.T) {
	ctx := context.Background()
	catalog := &countingCatalog{stub: marketplace.Stub{OrderSupported: true}}
	cached := New(catalog, newMemoryCache(), time.Minute)

	order, err := cached.Order(ctx, "coffee-machine", "Москва")
	if err != nil {
		t.Fatalf("заказ: %v", err)
	}
	if order == "" {
		t.Error("идентификатор заказа не возвращён")
	}

	t.Run("недоступная площадка", func(t *testing.T) {
		// Оформление заказа у площадки может быть недоступно, и это
		// честный отказ, а не имитация покупки.
		unavailable := New(&countingCatalog{stub: marketplace.Stub{Unavailable: true}},
			newMemoryCache(), time.Minute)
		if _, err := unavailable.Order(ctx, "coffee-machine", "Москва"); err == nil {
			t.Error("заказ у недоступной площадки прошёл")
		}
	})
}

func TestBuild(t *testing.T) {
	tests := []struct {
		name      string
		providers []string
		wantErr   bool
	}{
		{"заглушка", []string{"STUB"}, false},
		{"пустой список", nil, false},
		// Адаптеры площадок — T-076: их нельзя написать, не выяснив,
		// что доступно стороннему приложению.
		{"OZON пока не реализован", []string{"OZON"}, true},
		{"WB пока не реализован", []string{"WB"}, true},
		{"неизвестная площадка", []string{"WHATEVER"}, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry, err := Build(test.providers, newMemoryCache(), time.Minute)
			if test.wantErr {
				if err == nil {
					t.Error("список принят, ожидался отказ")
				}
				return
			}
			if err != nil {
				t.Fatalf("сборка реестра: %v", err)
			}
			if registry == nil {
				t.Error("реестр не создан")
			}
		})
	}
}

// TestBuildWrapsWithCache проверяет то, ради чего Build существует: каждая
// площадка оборачивается кэшем, иначе обращение уходит на неё при каждом
// показе списка.
func TestBuildWrapsWithCache(t *testing.T) {
	ctx := context.Background()
	cache := newMemoryCache()

	registry, err := Build([]string{"STUB"}, cache, time.Minute)
	if err != nil {
		t.Fatalf("сборка реестра: %v", err)
	}
	catalog, err := registry.Catalog(marketplace.ProviderStub)
	if err != nil {
		t.Fatalf("площадка не найдена в реестре: %v", err)
	}
	if _, err := catalog.Product(ctx, "coffee-machine"); err != nil {
		t.Fatalf("карточка товара: %v", err)
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.values) == 0 {
		t.Error("карточка не попала в кэш: площадка подключена без обёртки")
	}
}
