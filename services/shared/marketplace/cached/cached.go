// Package cached добавляет к площадке кэш и размыкатель цепи.
//
// Отдельный пакет, потому что кэш и размыкатель живут в инфраструктурном
// services, а модель товара нужна и веб-интерфейсу: под WebAssembly импорт
// services тянет драйвер PostgreSQL, gRPC и протобуф — двадцать восемь
// мегабайт вместо трёх.
package cached

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"wish/services"
	"wish/services/shared/marketplace"
)

// Cached оборачивает площадку кэшем и размыкателем цепи.
//
// Кэш здесь не про скорость: у публичных API есть ограничения частоты,
// и дёргать площадку на каждый показ списка желаний нельзя.
type Cached struct {
	catalog marketplace.Catalog
	cache   services.Cache
	breaker *services.Breaker
	ttl     time.Duration
}

func New(catalog marketplace.Catalog, cache services.Cache, ttl time.Duration) *Cached {
	return &Cached{
		catalog: catalog,
		cache:   cache,
		ttl:     ttl,
		// Размыкается только на недоступность площадки: отсутствие товара —
		// это нормальный ответ, а не отказ зависимости.
		breaker: services.NewBreaker(
			"marketplace-"+string(catalog.Provider()), 5, 30*time.Second,
			func(err error) bool { return errors.Is(err, marketplace.ErrUnavailable) },
		),
	}
}

func (c *Cached) Provider() marketplace.Provider {
	return c.catalog.Provider()
}

func (c *Cached) Product(ctx context.Context, id string) (marketplace.Product, error) {
	key := fmt.Sprintf("marketplace:%s:%s", c.catalog.Provider(), id)

	if cached, err := c.cache.Get(ctx, key); err == nil {
		var product marketplace.Product
		if err = json.Unmarshal([]byte(cached), &product); err == nil {
			return product, nil
		}
		slog.WarnContext(ctx, "Discarding malformed cached product",
			slog.String("key", key), slog.String("err", err.Error()))
	}

	var product marketplace.Product
	err := c.breaker.Do(ctx, func(ctx context.Context) error {
		fetched, err := c.catalog.Product(ctx, id)
		if err != nil {
			return err
		}
		product = fetched
		return nil
	})
	if err != nil {
		return marketplace.Product{}, err
	}

	encoded, err := json.Marshal(product)
	if err != nil {
		// Промах кэша не повод терять уже полученный ответ.
		slog.ErrorContext(ctx, "Can't encode product for cache", slog.String("err", err.Error()))
		return product, nil
	}
	if err = c.cache.SetTtl(ctx, key, string(encoded), c.ttl); err != nil {
		slog.WarnContext(ctx, "Can't cache product", slog.String("err", err.Error()))
	}
	return product, nil
}

func (c *Cached) Order(ctx context.Context, id string, address string) (string, error) {
	var order string
	err := c.breaker.Do(ctx, func(ctx context.Context) error {
		created, err := c.catalog.Order(ctx, id, address)
		if err != nil {
			return err
		}
		order = created
		return nil
	})
	return order, err
}

// Build собирает реестр подключённых площадок.
//
// Каждая оборачивается кэшем: у публичных API есть ограничения частоты,
// и обращаться к площадке на каждый показ списка или сверку цен нельзя.
func Build(providers []string, cache services.Cache, ttl time.Duration) (*marketplace.Registry, error) {
	catalogs := make([]marketplace.Catalog, 0, len(providers))
	for _, provider := range providers {
		switch marketplace.Provider(provider) {
		case marketplace.ProviderStub:
			catalogs = append(catalogs, New(&marketplace.Stub{}, cache, ttl))
		case marketplace.ProviderOzon, marketplace.ProviderWildberry:
			// Адаптеры площадок — T-076: их нельзя написать, не выяснив,
			// что доступно стороннему приложению (ADR 0004).
			return nil, fmt.Errorf("marketplace %s is not implemented yet", provider)
		default:
			return nil, fmt.Errorf("unknown marketplace %q", provider)
		}
	}
	return marketplace.NewRegistry(catalogs...), nil
}
