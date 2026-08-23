package marketplace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"wish/services"
)

// Cached оборачивает площадку кэшем и размыкателем цепи.
//
// Кэш здесь не про скорость: у публичных API есть ограничения частоты,
// и дёргать площадку на каждый показ списка желаний нельзя.
type Cached struct {
	catalog Catalog
	cache   services.Cache
	breaker *services.Breaker
	ttl     time.Duration
}

func NewCached(catalog Catalog, cache services.Cache, ttl time.Duration) *Cached {
	return &Cached{
		catalog: catalog,
		cache:   cache,
		ttl:     ttl,
		// Размыкается только на недоступность площадки: отсутствие товара —
		// это нормальный ответ, а не отказ зависимости.
		breaker: services.NewBreaker(
			"marketplace-"+string(catalog.Provider()), 5, 30*time.Second,
			func(err error) bool { return errors.Is(err, ErrUnavailable) },
		),
	}
}

func (c *Cached) Provider() Provider {
	return c.catalog.Provider()
}

func (c *Cached) Product(ctx context.Context, id string) (Product, error) {
	key := fmt.Sprintf("marketplace:%s:%s", c.catalog.Provider(), id)

	if cached, err := c.cache.Get(ctx, key); err == nil {
		var product Product
		if err = json.Unmarshal([]byte(cached), &product); err == nil {
			return product, nil
		}
		slog.WarnContext(ctx, "Discarding malformed cached product",
			slog.String("key", key), slog.String("err", err.Error()))
	}

	var product Product
	err := c.breaker.Do(ctx, func(ctx context.Context) error {
		fetched, err := c.catalog.Product(ctx, id)
		if err != nil {
			return err
		}
		product = fetched
		return nil
	})
	if err != nil {
		return Product{}, err
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
