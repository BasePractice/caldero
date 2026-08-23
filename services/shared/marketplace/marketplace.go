// Package marketplace описывает работу с внешними торговыми площадками.
package marketplace

import (
	"context"
	"errors"
	"fmt"
	"time"

	"wish/services"
	"wish/services/shared/credit"
)

// Ошибки, общие для всех площадок.
var (
	// ErrNotFound — товара с таким идентификатором нет.
	ErrNotFound = errors.New("product not found")
	// ErrUnavailable — площадка недоступна. Отличать её от прочих ошибок
	// нужно, чтобы не ронять запрос целиком: список желаний должен
	// открываться и без цен.
	ErrUnavailable = errors.New("marketplace is unavailable")
	// ErrUnsupported — площадка не поддерживает операцию. Оформление заказа
	// от имени покупателя может быть недоступно в принципе.
	ErrUnsupported = errors.New("operation is not supported by the marketplace")
)

// Provider — идентификатор площадки.
type Provider string

const (
	ProviderOzon      Provider = "OZON"
	ProviderWildberry Provider = "WB"
	// ProviderStub — заглушка для локальной разработки и тестов.
	ProviderStub Provider = "STUB"
)

// Product — карточка товара.
type Product struct {
	Provider Provider      `json:"provider"`
	Id       string        `json:"id"`
	Title    string        `json:"title"`
	Price    credit.Amount `json:"price"`
	URL      string        `json:"url"`
	Image    string        `json:"image,omitempty"`
	InStock  bool          `json:"in_stock"`
	// FetchedAt — когда карточка получена. Цена на площадке меняется,
	// и без отметки времени непонятно, насколько снимок устарел.
	FetchedAt time.Time `json:"fetched_at"`
}

// Catalog — то, что нужно от площадки. Интерфейс объявлен здесь,
// у потребителя: площадке незачем знать, кто и как ей пользуется.
type Catalog interface {
	// Product возвращает карточку по идентификатору.
	Product(ctx context.Context, id string) (Product, error)
	// Order оформляет заказ. Может вернуть ErrUnsupported: публичные API
	// площадок ориентированы на продавца, и оформление от имени покупателя
	// доступно не везде.
	Order(ctx context.Context, id string, address string) (string, error)
	// Provider сообщает, какая это площадка.
	Provider() Provider
}

// Registry подбирает площадку по идентификатору провайдера.
type Registry struct {
	catalogs map[Provider]Catalog
}

func NewRegistry(catalogs ...Catalog) *Registry {
	registry := &Registry{catalogs: make(map[Provider]Catalog, len(catalogs))}
	for _, catalog := range catalogs {
		registry.catalogs[catalog.Provider()] = catalog
	}
	return registry
}

func (r *Registry) Catalog(provider Provider) (Catalog, error) {
	catalog, ok := r.catalogs[provider]
	if !ok {
		return nil, fmt.Errorf("marketplace %s is not configured", provider)
	}
	return catalog, nil
}

// Providers перечисляет подключённые площадки.
func (r *Registry) Providers() []Provider {
	providers := make([]Provider, 0, len(r.catalogs))
	for provider := range r.catalogs {
		providers = append(providers, provider)
	}
	return providers
}

// Build собирает реестр подключённых площадок.
//
// Каждая оборачивается кэшем: у публичных API есть ограничения частоты,
// и обращаться к площадке на каждый показ списка или сверку цен нельзя.
func Build(providers []string, cache services.Cache, ttl time.Duration) (*Registry, error) {
	catalogs := make([]Catalog, 0, len(providers))
	for _, provider := range providers {
		switch Provider(provider) {
		case ProviderStub:
			catalogs = append(catalogs, NewCached(&Stub{}, cache, ttl))
		case ProviderOzon, ProviderWildberry:
			// Адаптеры площадок — T-076: их нельзя написать, не выяснив,
			// что доступно стороннему приложению (ADR 0004).
			return nil, fmt.Errorf("marketplace %s is not implemented yet", provider)
		default:
			return nil, fmt.Errorf("unknown marketplace %q", provider)
		}
	}
	return NewRegistry(catalogs...), nil
}
