package marketplace

import (
	"context"
	"errors"
	"testing"
)

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
