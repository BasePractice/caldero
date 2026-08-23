package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"

	"wish/services/shared/credit"
	"wish/services/shared/marketplace"
	"wish/services/shared/pick"
	"wish/services/shared/wallets"
	"wish/services/shared/wishlist"

	"github.com/google/uuid"
)

// ErrShopUnavailable — платить за покупки некуда: кошелёк площадки
// не настроен. Списывать средства «в никуда» нельзя — сумма денег
// в системе перестала бы сходиться.
var ErrShopUnavailable = errors.New("marketplace wallet is not configured")

// historyLimit ограничивает выдачу истории прогонов.
const historyLimit = 50

// Shopaholic покупает случайный набор товаров в пределах бюджета.
//
// Живёт в сервисе списка желаний, а не в отдельном: ему нужны те же
// зависимости — площадка и кошелёк — и те же данные о товарах
// пользователя. Отдельный сервис ради одной операции добавил бы узел
// в инфраструктуру, не добавив границы.
type Shopaholic struct {
	db       Database
	catalogs *marketplace.Registry
	wallet   Wallet
	// shop — кошелёк площадки: покупка это уход средств из системы
	// к продавцу, и у этого ухода должен быть адресат.
	shop *uuid.UUID
}

func NewShopaholic(
	db Database,
	catalogs *marketplace.Registry,
	wallet Wallet,
	shop *uuid.UUID,
) *Shopaholic {
	return &Shopaholic{db: db, catalogs: catalogs, wallet: wallet, shop: shop}
}

// Shop проводит прогон: отбирает набор в пределах бюджета, заказывает
// его и платит за то, что удалось заказать.
//
// Отказ площадки в середине списка не отменяет уже оформленное и не
// списывает за неоформленное: каждый товар заказывается и оплачивается
// отдельно, а итог показывает, что именно не состоялось.
func (s *Shopaholic) Shop(
	ctx context.Context,
	user uuid.UUID,
	request wishlist.StartShopping,
) (wishlist.Run, error) {
	if err := request.Validate(); err != nil {
		return wishlist.Run{}, err
	}
	if s.wallet == nil {
		return wishlist.Run{}, fmt.Errorf("%w: wallet service is not configured", ErrWalletUnavailable)
	}
	if s.shop == nil {
		return wishlist.Run{}, ErrShopUnavailable
	}

	products, err := s.catalog(ctx, request.Items)
	if err != nil {
		return wishlist.Run{}, err
	}

	// Зерно из crypto/rand: набор должен быть непредсказуемым, иначе
	// «случайные покупки» становятся предсказуемыми для того, кто знает
	// момент запуска.
	seed := make([]byte, 32)
	if _, err = rand.Read(seed); err != nil {
		return wishlist.Run{}, fmt.Errorf("generating shopping seed: %w", err)
	}

	run, err := s.db.StartRun(ctx, user, request.Budget, seed)
	if err != nil {
		return wishlist.Run{}, err
	}

	// Правило отбора общее для всей системы — см. pick.Within.
	selected, total := pick.Within(seed, "shopping", products,
		func(product marketplace.Product) string {
			return string(product.Provider) + ":" + product.Id
		},
		func(product marketplace.Product) credit.Amount { return product.Price },
		request.Budget)
	if len(selected) == 0 {
		// В бюджет не поместилось ничего: средства не тронуты.
		return s.db.FinishRun(ctx, run.Id, 0, wishlist.RunEmpty)
	}

	source, err := s.wallet.Wallet(ctx, user)
	if err != nil {
		return wishlist.Run{}, fmt.Errorf("%w: %s", ErrWalletUnavailable, err)
	}
	if source.Available < total {
		// Проверка до заказов: заказать и не оплатить хуже, чем
		// не заказать вовсе.
		if _, err = s.db.FinishRun(ctx, run.Id, 0, wishlist.RunEmpty); err != nil {
			slog.ErrorContext(ctx, "Can't finish empty run", slog.String("err", err.Error()))
		}
		return wishlist.Run{}, fmt.Errorf("%w: available %s, selected %s",
			ErrInsufficientFunds, source.Available, total)
	}

	spent, ordered := s.buy(ctx, run.Id, user, source.Id, selected)

	state := wishlist.RunDone
	switch {
	case ordered == 0:
		state = wishlist.RunEmpty
	case ordered < len(selected):
		state = wishlist.RunPartial
	}
	return s.db.FinishRun(ctx, run.Id, spent, state)
}

// buy заказывает и оплачивает отобранное, возвращая потраченное
// и число оформленных заказов.
func (s *Shopaholic) buy(
	ctx context.Context,
	run, user, source uuid.UUID,
	products []marketplace.Product,
) (credit.Amount, int) {
	var (
		spent   credit.Amount
		ordered int
	)
	for _, product := range products {
		purchase, err := s.db.AddPurchase(ctx, wishlist.Purchase{
			RunId: run, Provider: product.Provider, ProductId: product.Id,
			Title: product.Title, URL: product.URL, Price: product.Price,
		})
		if err != nil {
			slog.ErrorContext(ctx, "Can't record purchase", slog.String("err", err.Error()))
			continue
		}

		catalog, err := s.catalogs.Catalog(product.Provider)
		if err != nil {
			s.settle(ctx, purchase.Id, false, false, "", err.Error())
			continue
		}
		order, err := catalog.Order(ctx, product.Id, "")
		if err != nil {
			// Отказ по одному товару не отменяет остальные: список
			// кандидатов на то и список.
			s.settle(ctx, purchase.Id, false, false, "", err.Error())
			continue
		}

		if err = s.wallet.Transfer(ctx, user, wallets.TransferParams{
			IdempotencyKey: fmt.Sprintf("shopping:%s:%s:%s", run, product.Provider, product.Id),
			Source:         source,
			Target:         *s.shop,
			Value:          product.Price,
			Message:        "Покупка: " + product.Title,
		}); err != nil {
			// Заказ оформлен, а оплата не прошла. Дальше не идём:
			// проблема не в товаре, а в деньгах, и следующие заказы
			// повторят её.
			slog.ErrorContext(ctx, "Ordered but not paid",
				slog.String("product", product.Id), slog.String("err", err.Error()))
			s.settle(ctx, purchase.Id, true, false, order, err.Error())
			ordered++
			break
		}

		s.settle(ctx, purchase.Id, true, true, order, "")
		spent += product.Price
		ordered++
	}
	return spent, ordered
}

func (s *Shopaholic) settle(
	ctx context.Context,
	purchase uuid.UUID,
	ordered, paid bool,
	order, failure string,
) {
	if err := s.db.SettlePurchase(ctx, purchase, ordered, paid, order, failure); err != nil {
		slog.ErrorContext(ctx, "Can't settle purchase",
			slog.String("purchase", purchase.String()), slog.String("err", err.Error()))
	}
}

// catalog собирает карточки кандидатов. Товар, которого больше нет,
// просто не участвует в отборе; недоступность площадки — другое дело:
// без цен отбирать нечего.
func (s *Shopaholic) catalog(
	ctx context.Context,
	items []wishlist.ShoppingItem,
) ([]marketplace.Product, error) {
	if s.catalogs == nil {
		return nil, fmt.Errorf("%w: no marketplace is configured", ErrMarketplaceUnavailable)
	}

	products := make([]marketplace.Product, 0, len(items))
	for _, item := range items {
		catalog, err := s.catalogs.Catalog(item.Provider)
		if err != nil {
			return nil, fmt.Errorf("%w: %s", ErrProductNotFound, err)
		}
		product, err := catalog.Product(ctx, item.ProductId)
		switch {
		case errors.Is(err, marketplace.ErrNotFound):
			slog.DebugContext(ctx, "Candidate is gone from the marketplace",
				slog.String("product", item.ProductId))
			continue
		case err != nil:
			return nil, fmt.Errorf("%w: %s", ErrMarketplaceUnavailable, err)
		}
		products = append(products, product)
	}
	return products, nil
}

// Run отдаёт прогон владельцу.
func (s *Shopaholic) Run(ctx context.Context, user, id uuid.UUID) (wishlist.Run, error) {
	run, err := s.db.Run(ctx, id)
	if err != nil {
		return wishlist.Run{}, err
	}
	if run.UserId != user {
		// Чужой прогон отдаётся как несуществующий: он показывает,
		// что человек покупает и сколько тратит.
		return wishlist.Run{}, ErrNotFound
	}
	return run, nil
}

// History отдаёт последние прогоны пользователя.
func (s *Shopaholic) History(ctx context.Context, user uuid.UUID) ([]wishlist.Run, error) {
	return s.db.Runs(ctx, user, historyLimit)
}
