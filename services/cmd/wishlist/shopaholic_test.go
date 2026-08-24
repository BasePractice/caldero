package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"wish/services/shared/credit"
	"wish/services/shared/marketplace"
	"wish/services/shared/payment"
	"wish/services/shared/wishlist"

	"github.com/google/uuid"
)

// flakyCatalog отказывает в заказе после нескольких успешных: именно так
// выглядит отказ площадки в середине списка.
type flakyCatalog struct {
	mu       sync.Mutex
	stub     marketplace.Stub
	succeeds int
	orders   int
}

func (f *flakyCatalog) Provider() marketplace.Provider { return marketplace.ProviderStub }

func (f *flakyCatalog) Product(ctx context.Context, id string) (marketplace.Product, error) {
	return f.stub.Product(ctx, id)
}

func (f *flakyCatalog) Order(_ context.Context, id, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.orders++
	if f.orders > f.succeeds {
		return "", fmt.Errorf("%w: площадка отказала", marketplace.ErrUnavailable)
	}
	return "order-" + id, nil
}

// shoppingItems подбирает кандидатов не дороже предела: заглушка выводит
// цену из идентификатора, и дешёвые товары ищутся перебором.
func shoppingItems(t *testing.T, count int, limit credit.Amount) []wishlist.ShoppingItem {
	t.Helper()

	stub := &marketplace.Stub{}
	items := make([]wishlist.ShoppingItem, 0, count)
	for i := range 2000 {
		id := fmt.Sprintf("shop-%d", i)
		product, err := stub.Product(context.Background(), id)
		if err != nil {
			t.Fatalf("карточка товара: %v", err)
		}
		if product.Price > limit {
			continue
		}
		items = append(items, wishlist.ShoppingItem{
			Provider: marketplace.ProviderStub, ProductId: id,
		})
		if len(items) == count {
			return items
		}
	}
	t.Fatalf("не нашлось %d товаров дешевле %s", count, limit)
	return nil
}

func TestShoppingStaysWithinBudget(t *testing.T) {
	ctx := context.Background()
	env := newTestEnvironment(t, payment.Fee{}, nil)
	env.stub.OrderSupported = true
	user := uuid.New()
	env.wallet.fund(user, 1_000_000_00)

	items := shoppingItems(t, 8, 5_000_00)
	budget := credit.Amount(10_000_00)

	for range 10 {
		run, err := env.shopaholic.Shop(ctx, user, wishlist.StartShopping{
			Budget: budget, Items: items,
		})
		if err != nil {
			t.Fatalf("прогон шопоголика: %v", err)
		}
		if run.Spent > budget {
			t.Fatalf("потрачено %s при бюджете %s", run.Spent, budget)
		}

		var sum credit.Amount
		for _, purchase := range run.Purchases {
			if purchase.Paid {
				sum += purchase.Price
			}
		}
		if sum != run.Spent {
			t.Fatalf("итог %s не совпал с суммой оплаченного %s", run.Spent, sum)
		}
	}
}

func TestShoppingPaysMarketplace(t *testing.T) {
	ctx := context.Background()
	env := newTestEnvironment(t, payment.Fee{}, nil)
	env.stub.OrderSupported = true
	user := uuid.New()
	env.wallet.fund(user, 100_000_00)
	before := env.wallet.total()

	run, err := env.shopaholic.Shop(ctx, user, wishlist.StartShopping{
		Budget: 20_000_00, Items: shoppingItems(t, 4, 5_000_00),
	})
	if err != nil {
		t.Fatalf("прогон шопоголика: %v", err)
	}
	if run.State != wishlist.RunDone {
		t.Fatalf("состояние %s, ожидалось %s (%+v)", run.State, wishlist.RunDone, run.Purchases)
	}
	if run.Spent <= 0 {
		t.Fatal("ничего не куплено")
	}

	// Покупка — это уход средств к продавцу: они не исчезают, а лежат
	// на кошельке площадки.
	if env.wallet.balanceOf(user) != 100_000_00-run.Spent {
		t.Errorf("у покупателя осталось %s", env.wallet.balanceOf(user))
	}
	if env.wallet.walletBalance(env.shop) != run.Spent {
		t.Errorf("площадке ушло %s, потрачено %s", env.wallet.walletBalance(env.shop), run.Spent)
	}
	if env.wallet.total() != before {
		t.Errorf("сумма средств в системе изменилась: было %s, стало %s", before, env.wallet.total())
	}

	for _, purchase := range run.Purchases {
		if !purchase.Ordered || !purchase.Paid || purchase.OrderId == "" {
			t.Errorf("покупка не доведена до конца: %+v", purchase)
		}
	}
}

// TestShoppingPartialFailure проверяет главное требование по устойчивости:
// отказ площадки в середине списка не приводит к списанию за неоформленное.
func TestShoppingPartialFailure(t *testing.T) {
	ctx := context.Background()
	env := newTestEnvironment(t, payment.Fee{}, nil)
	user := uuid.New()
	env.wallet.fund(user, 100_000_00)

	flaky := &flakyCatalog{succeeds: 1}
	shop := uuid.New()
	env.shopaholic = NewShopaholic(env.db, marketplace.NewRegistry(flaky), env.wallet, &shop)

	run, err := env.shopaholic.Shop(ctx, user, wishlist.StartShopping{
		Budget: 20_000_00, Items: shoppingItems(t, 4, 3_000_00),
	})
	if err != nil {
		t.Fatalf("прогон шопоголика: %v", err)
	}
	if run.State != wishlist.RunPartial {
		t.Fatalf("состояние %s, ожидалось %s", run.State, wishlist.RunPartial)
	}

	var ordered, failed int
	var expected credit.Amount
	for _, purchase := range run.Purchases {
		if purchase.Ordered {
			ordered++
			expected += purchase.Price
			continue
		}
		failed++
		if purchase.Paid {
			t.Errorf("оплачен неоформленный заказ: %+v", purchase)
		}
		if purchase.Failure == "" {
			t.Errorf("не сказано, почему заказ не прошёл: %+v", purchase)
		}
	}
	if ordered != 1 || failed == 0 {
		t.Fatalf("оформлено %d заказов, отказов %d", ordered, failed)
	}

	// Списано ровно за оформленное, а не за весь отобранный набор.
	if run.Spent != expected {
		t.Errorf("списано %s, ожидалось %s", run.Spent, expected)
	}
	if env.wallet.balanceOf(user) != 100_000_00-expected {
		t.Errorf("у покупателя осталось %s", env.wallet.balanceOf(user))
	}
	if env.wallet.walletBalance(shop) != expected {
		t.Errorf("площадке ушло %s", env.wallet.walletBalance(shop))
	}
}

// TestShoppingWithoutOrdering фиксирует нынешнюю реальность: публичные API
// площадок не дают оформить заказ от имени покупателя (EXT-03), и тогда
// шопоголик не тратит ничего.
func TestShoppingWithoutOrdering(t *testing.T) {
	ctx := context.Background()
	env := newTestEnvironment(t, payment.Fee{}, nil)
	user := uuid.New()
	env.wallet.fund(user, 100_000_00)

	run, err := env.shopaholic.Shop(ctx, user, wishlist.StartShopping{
		Budget: 20_000_00, Items: shoppingItems(t, 3, 3_000_00),
	})
	if err != nil {
		t.Fatalf("прогон шопоголика: %v", err)
	}
	if run.State != wishlist.RunEmpty || run.Spent != 0 {
		t.Errorf("состояние %s, потрачено %s", run.State, run.Spent)
	}
	if env.wallet.balanceOf(user) != 100_000_00 {
		t.Errorf("средства списаны без заказов: %s", env.wallet.balanceOf(user))
	}
}

func TestShoppingChecksFunds(t *testing.T) {
	ctx := context.Background()
	env := newTestEnvironment(t, payment.Fee{}, nil)
	env.stub.OrderSupported = true
	user := uuid.New()
	env.wallet.fund(user, 100_00)

	_, err := env.shopaholic.Shop(ctx, user, wishlist.StartShopping{
		Budget: 20_000_00, Items: shoppingItems(t, 3, 3_000_00),
	})
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("получено %v, ожидалась %v", err, ErrInsufficientFunds)
	}
	// Проверка до заказов: заказать и не оплатить хуже, чем не заказать.
	if env.wallet.balanceOf(user) != 100_00 {
		t.Errorf("баланс изменился: %s", env.wallet.balanceOf(user))
	}
}

func TestShoppingWithoutMarketplaceWallet(t *testing.T) {
	ctx := context.Background()
	env := newTestEnvironment(t, payment.Fee{}, nil)
	env.shopaholic = NewShopaholic(env.db, marketplace.NewRegistry(env.stub), env.wallet, nil)
	user := uuid.New()
	env.wallet.fund(user, 100_000_00)

	// Списывать «в никуда» нельзя: сумма денег в системе перестала бы
	// сходиться.
	if _, err := env.shopaholic.Shop(ctx, user, wishlist.StartShopping{
		Budget: 10_000_00, Items: shoppingItems(t, 2, 3_000_00),
	}); !errors.Is(err, ErrShopUnavailable) {
		t.Errorf("получено %v, ожидалась %v", err, ErrShopUnavailable)
	}
}

func TestShoppingBudgetTooSmall(t *testing.T) {
	ctx := context.Background()
	env := newTestEnvironment(t, payment.Fee{}, nil)
	env.stub.OrderSupported = true
	user := uuid.New()
	env.wallet.fund(user, 100_000_00)

	// В бюджет не помещается ни один товар.
	run, err := env.shopaholic.Shop(ctx, user, wishlist.StartShopping{
		Budget: 100, Items: shoppingItems(t, 3, 3_000_00),
	})
	if err != nil {
		t.Fatalf("прогон шопоголика: %v", err)
	}
	if run.State != wishlist.RunEmpty || run.Spent != 0 {
		t.Errorf("состояние %s, потрачено %s", run.State, run.Spent)
	}
	if env.wallet.balanceOf(user) != 100_000_00 {
		t.Errorf("средства тронуты: %s", env.wallet.balanceOf(user))
	}
}

func TestShoppingValidation(t *testing.T) {
	tests := []struct {
		name    string
		request wishlist.StartShopping
	}{
		{"без бюджета", wishlist.StartShopping{
			Items: []wishlist.ShoppingItem{{Provider: marketplace.ProviderStub, ProductId: "a"}}}},
		{"без товаров", wishlist.StartShopping{Budget: 1_000_00}},
		{"товар без площадки", wishlist.StartShopping{Budget: 1_000_00,
			Items: []wishlist.ShoppingItem{{ProductId: "a"}}}},
		{"слишком длинный список", wishlist.StartShopping{Budget: 1_000_00,
			Items: make([]wishlist.ShoppingItem, wishlist.MaxShoppingItems+1)}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.request.Validate(); err == nil {
				t.Error("запрос принят")
			}
		})
	}
}

func TestShoppingHistoryIsPrivate(t *testing.T) {
	ctx := context.Background()
	env := newTestEnvironment(t, payment.Fee{}, nil)
	env.stub.OrderSupported = true
	user := uuid.New()
	env.wallet.fund(user, 100_000_00)

	run, err := env.shopaholic.Shop(ctx, user, wishlist.StartShopping{
		Budget: 10_000_00, Items: shoppingItems(t, 2, 3_000_00),
	})
	if err != nil {
		t.Fatalf("прогон шопоголика: %v", err)
	}

	history, err := env.shopaholic.History(ctx, user)
	if err != nil {
		t.Fatalf("история покупок: %v", err)
	}
	if len(history) != 1 {
		t.Errorf("прогонов в истории %d, ожидался 1", len(history))
	}

	// Чужой прогон показывает, что человек покупает и сколько тратит.
	if _, err = env.shopaholic.Run(ctx, uuid.New(), run.Id); !errors.Is(err, ErrNotFound) {
		t.Errorf("получено %v, ожидалась %v", err, ErrNotFound)
	}
}

// TestShoppingFromUnknownProvider: площадка, которой нет в реестре, —
// это ненайденный товар, а не сбой сервиса. Без разбора этой ветки
// клиент получил бы 500 на опечатку в названии площадки.
func TestShoppingFromUnknownProvider(t *testing.T) {
	env := newTestEnvironment(t, payment.Fee{}, nil)
	buyer := uuid.New()
	env.wallet.fund(buyer, 1_000_000)

	_, err := env.shopaholic.Shop(t.Context(), buyer, wishlist.StartShopping{
		Budget: 500_000,
		Items:  []wishlist.ShoppingItem{{Provider: "WHATEVER", ProductId: "x"}},
	})
	if !errors.Is(err, ErrProductNotFound) {
		t.Errorf("получено %v, ожидалась ErrProductNotFound", err)
	}
}

// TestShoppingWithUnavailableMarketplace: без цен отбирать нечего,
// и прогон обязан честно отказать, а не купить наугад.
func TestShoppingWithUnavailableMarketplace(t *testing.T) {
	env := newTestEnvironment(t, payment.Fee{}, nil)
	env.stub.Unavailable = true
	buyer := uuid.New()
	env.wallet.fund(buyer, 1_000_000)

	_, err := env.shopaholic.Shop(t.Context(), buyer, wishlist.StartShopping{
		Budget: 500_000,
		Items:  []wishlist.ShoppingItem{{Provider: marketplace.ProviderStub, ProductId: "coffee-machine"}},
	})
	if !errors.Is(err, ErrMarketplaceUnavailable) {
		t.Errorf("получено %v, ожидалась ErrMarketplaceUnavailable", err)
	}
}
