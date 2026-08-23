package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"wish/services/shared/credit"
	"wish/services/shared/marketplace"
	"wish/services/shared/notify"
	"wish/services/shared/payment"
	"wish/services/shared/wishlist"

	"github.com/google/uuid"
)

// Ошибки операций. Отделены от сбоев БД: каждая — это конкретный ответ
// клиенту, а не отказ сервиса.
var (
	// ErrForbidden — операция над чужим элементом.
	ErrForbidden = errors.New("item belongs to another user")
	// ErrMarketplaceUnavailable — площадка не ответила. Добавить товар
	// с непроверенной ценой нельзя: цена уходит в котёл подарков.
	ErrMarketplaceUnavailable = errors.New("marketplace is unavailable")
	// ErrProductNotFound — товара с таким идентификатором нет.
	ErrProductNotFound = errors.New("product not found")
	// ErrWalletUnavailable — кошелёк недоступен, перевод не выполнен.
	ErrWalletUnavailable = errors.New("wallet is unavailable")
	// ErrInsufficientFunds — у дарителя не хватает средств на подарок
	// вместе с комиссией.
	ErrInsufficientFunds = errors.New("insufficient funds")
)

// WalletInfo — то, что нужно знать о кошельке.
type WalletInfo struct {
	Id uuid.UUID
	// Available — остаток за вычетом действующих резервов.
	Available credit.Amount
}

// TransferParams — перевод между кошельками.
type TransferParams struct {
	IdempotencyKey string
	Source         uuid.UUID
	Target         uuid.UUID
	Value          credit.Amount
	Message        string
}

// Wallet — то, что нужно от сервиса кошелька. Интерфейс объявлен здесь,
// у потребителя.
type Wallet interface {
	// Wallet возвращает кошелёк пользователя. Вызывается и для чужого
	// кошелька, поэтому реализация ходит от имени служебного оператора.
	Wallet(ctx context.Context, user uuid.UUID) (WalletInfo, error)
	// Transfer переводит средства от имени владельца исходного кошелька:
	// подарок делает даритель, а не система за него.
	Transfer(ctx context.Context, giver uuid.UUID, params TransferParams) error
}

// Gifts — операции над списком желаний вместе с их последствиями:
// оповещениями, заказом на площадке и переводом средств.
type Gifts struct {
	db         Database
	catalogs   *marketplace.Registry
	notifier   *notify.Client
	wallet     Wallet
	fee        payment.Fee
	feeWallet  *uuid.UUID
	reserveTTL time.Duration
}

func NewGifts(
	db Database,
	catalogs *marketplace.Registry,
	notifier *notify.Client,
	wallet Wallet,
	fee payment.Fee,
	feeWallet *uuid.UUID,
	reserveTTL time.Duration,
) *Gifts {
	return &Gifts{
		db: db, catalogs: catalogs, notifier: notifier, wallet: wallet,
		fee: fee, feeWallet: feeWallet, reserveTTL: reserveTTL,
	}
}

// Add добавляет элемент в список. Название и цена товара берутся
// из карточки площадки, а не из запроса: иначе в списке окажется
// товар с выдуманной ценой, и по ней будет считаться котёл подарков.
func (g *Gifts) Add(
	ctx context.Context,
	owner uuid.UUID,
	create wishlist.CreateItem,
) (wishlist.Item, error) {
	item := wishlist.Item{
		UserId:   owner,
		Kind:     create.Kind,
		State:    wishlist.StateVisible,
		Priority: create.Priority,
		Comment:  create.Comment,
		Title:    create.Title,
	}

	if create.Kind == wishlist.KindMoney {
		item.Amount = create.Amount
		return g.db.Create(ctx, item)
	}

	catalog, err := g.catalogs.Catalog(create.Provider)
	if err != nil {
		return wishlist.Item{}, fmt.Errorf("%w: %s", ErrProductNotFound, err)
	}
	product, err := catalog.Product(ctx, create.ProductId)
	switch {
	case errors.Is(err, marketplace.ErrNotFound):
		return wishlist.Item{}, ErrProductNotFound
	case err != nil:
		// Недоступность площадки — не ошибка пользователя, и подставлять
		// вместо цены ноль нельзя: элемент попадёт в расчёты как бесплатный.
		return wishlist.Item{}, fmt.Errorf("%w: %s", ErrMarketplaceUnavailable, err)
	}

	fetched := product.FetchedAt
	item.Title = product.Title
	item.Provider = product.Provider
	item.ProductId = product.Id
	item.URL = product.URL
	item.Price = product.Price
	item.PriceAt = &fetched
	return g.db.Create(ctx, item)
}

// List отдаёт список пользователя глазами смотрящего.
func (g *Gifts) List(ctx context.Context, viewer, owner uuid.UUID) ([]wishlist.Item, error) {
	states := []wishlist.State(nil)
	if viewer != owner {
		// Чужой список показывается только в части того, что владелец
		// готов принять в подарок: скрытое, выбранное и отклонённое
		// дарителю знать незачем.
		states = []wishlist.State{wishlist.StateVisible}
	}

	items, err := g.db.Items(ctx, owner, states)
	if err != nil {
		return nil, err
	}
	for i, item := range items {
		items[i] = item.Public(viewer)
	}
	return items, nil
}

// Chosen отдаёт подарки, выбранные дарителем.
func (g *Gifts) Chosen(ctx context.Context, giver uuid.UUID) ([]wishlist.Item, error) {
	return g.db.Chosen(ctx, giver)
}

// Show и Hide переключают видимость элемента владельцем.
func (g *Gifts) Show(ctx context.Context, owner, id uuid.UUID) (wishlist.Item, error) {
	return g.ownerTransition(ctx, owner, id, wishlist.StateVisible)
}

func (g *Gifts) Hide(ctx context.Context, owner, id uuid.UUID) (wishlist.Item, error) {
	return g.ownerTransition(ctx, owner, id, wishlist.StateHidden)
}

func (g *Gifts) ownerTransition(
	ctx context.Context,
	owner, id uuid.UUID,
	to wishlist.State,
) (wishlist.Item, error) {
	if _, err := g.ownedItem(ctx, owner, id); err != nil {
		return wishlist.Item{}, err
	}
	return g.db.Transition(ctx, id, Transition{Actor: wishlist.ActorOwner, To: to})
}

// Reserve резервирует подарок за дарителем.
func (g *Gifts) Reserve(ctx context.Context, giver, id uuid.UUID) (wishlist.Item, error) {
	item, err := g.db.Item(ctx, id)
	if err != nil {
		return wishlist.Item{}, err
	}
	if item.UserId == giver {
		return wishlist.Item{}, fmt.Errorf("%w: cannot reserve an item from your own list", ErrForbidden)
	}
	// Чужой невидимый элемент для дарителя не существует.
	if item.State != wishlist.StateVisible {
		return wishlist.Item{}, ErrNotFound
	}

	until := time.Now().Add(g.reserveTTL)
	reserved, err := g.db.Transition(ctx, id, Transition{
		Actor:         wishlist.ActorGiver,
		To:            wishlist.StateChosen,
		Giver:         &giver,
		ReservedUntil: &until,
	})
	if err != nil {
		return wishlist.Item{}, err
	}

	// Одаряемый узнаёт, что «кто-то» хочет вручить подарок: имя дарителя
	// в оповещение не попадает, сюрприз входит в продукт.
	g.publish(ctx, reserved.UserId, notify.EventWishlistItemReserved, reserved,
		fmt.Sprintf("wishlist:%s:reserved:%d", reserved.Id, reserved.UpdatedAt.UnixNano()))
	return reserved, nil
}

// Cancel снимает резерв по решению дарителя.
func (g *Gifts) Cancel(ctx context.Context, giver, id uuid.UUID) (wishlist.Item, error) {
	return g.db.Transition(ctx, id, Transition{
		Actor: wishlist.ActorGiver,
		To:    wishlist.StateVisible,
		Giver: &giver,
	})
}

// Confirm — одаряемый согласен принять подарок.
func (g *Gifts) Confirm(ctx context.Context, owner, id uuid.UUID) (wishlist.Item, error) {
	item, err := g.decide(ctx, owner, id, wishlist.StateConfirmed)
	if err != nil {
		return wishlist.Item{}, err
	}
	if item.GiverId != nil {
		g.publish(ctx, *item.GiverId, notify.EventWishlistItemConfirmed, item,
			fmt.Sprintf("wishlist:%s:confirmed", item.Id))
	}
	return item, nil
}

// Reject — одаряемый отказался. По README отклонённый подарок больше
// не доступен к дарению, поэтому состояние терминальное.
func (g *Gifts) Reject(ctx context.Context, owner, id uuid.UUID) (wishlist.Item, error) {
	item, err := g.decide(ctx, owner, id, wishlist.StateRejected)
	if err != nil {
		return wishlist.Item{}, err
	}
	if item.GiverId != nil {
		g.publish(ctx, *item.GiverId, notify.EventWishlistItemRejected, item,
			fmt.Sprintf("wishlist:%s:rejected", item.Id))
	}
	return item, nil
}

func (g *Gifts) decide(
	ctx context.Context,
	owner, id uuid.UUID,
	to wishlist.State,
) (wishlist.Item, error) {
	if _, err := g.ownedItem(ctx, owner, id); err != nil {
		return wishlist.Item{}, err
	}
	return g.db.Transition(ctx, id, Transition{Actor: wishlist.ActorOwner, To: to})
}

// Accept завершает дарение: товар заказывается на площадке, деньги
// переводятся одаряемому.
func (g *Gifts) Accept(ctx context.Context, giver, id uuid.UUID) (wishlist.Item, error) {
	item, err := g.db.Item(ctx, id)
	if err != nil {
		return wishlist.Item{}, err
	}
	if item.GiverId == nil || *item.GiverId != giver {
		return wishlist.Item{}, fmt.Errorf("%w: item is reserved by another giver", ErrForbidden)
	}
	if err = wishlist.CanTransition(item.State, wishlist.StateAccepted, wishlist.ActorGiver); err != nil {
		return wishlist.Item{}, err
	}

	transition := Transition{Actor: wishlist.ActorGiver, To: wishlist.StateAccepted, Giver: &giver}
	switch item.Kind {
	case wishlist.KindMoney:
		if err = g.transfer(ctx, item, giver); err != nil {
			return wishlist.Item{}, err
		}
	case wishlist.KindProduct:
		// Заказ оформляется до смены состояния: акцепт означает, что
		// подарок вручён, и объявлять это раньше времени нельзя.
		if transition.OrderId, err = g.order(ctx, item); err != nil {
			return wishlist.Item{}, err
		}
	}

	accepted, err := g.db.Transition(ctx, id, transition)
	if err != nil {
		// Деньги уже переведены, а состояние не сменилось. Повтор акцепта
		// пройдёт тем же путём: ключ идемпотентности переводов выведен
		// из идентификатора элемента, и второй раз средства не спишутся.
		return wishlist.Item{}, err
	}

	g.publish(ctx, accepted.UserId, notify.EventWishlistItemGifted, accepted,
		fmt.Sprintf("wishlist:%s:gifted", accepted.Id))
	return accepted, nil
}

// order оформляет заказ на площадке. Отсутствие такой возможности —
// не ошибка: публичные API площадок ориентированы на продавца (ADR 0004),
// и подарок в этом случае заказывается дарителем вручную по ссылке.
func (g *Gifts) order(ctx context.Context, item wishlist.Item) (string, error) {
	catalog, err := g.catalogs.Catalog(item.Provider)
	if err != nil {
		slog.WarnContext(ctx, "Marketplace is not configured, order is left to the giver",
			slog.String("provider", string(item.Provider)))
		return "", nil
	}

	// Адрес доставки одаряемого система не хранит: заказ на площадке
	// оформляется на адрес, известный самой площадке.
	order, err := catalog.Order(ctx, item.ProductId, "")
	switch {
	case errors.Is(err, marketplace.ErrUnsupported):
		slog.InfoContext(ctx, "Marketplace does not support ordering, order is left to the giver",
			slog.String("provider", string(item.Provider)))
		return "", nil
	case err != nil:
		return "", fmt.Errorf("%w: %s", ErrMarketplaceUnavailable, err)
	}
	return order, nil
}

// transfer переводит денежный подарок одаряемому и удерживает комиссию.
//
// Распределённой транзакции здесь нет: кошелёк — отдельный сервис
// со своей базой. Порядок выбран так, чтобы худший исход был безопасным:
// сначала подарок, затем комиссия. Не удержанная комиссия — потеря системы,
// а не пользователя.
func (g *Gifts) transfer(ctx context.Context, item wishlist.Item, giver uuid.UUID) error {
	if g.wallet == nil {
		return fmt.Errorf("%w: wallet service is not configured", ErrWalletUnavailable)
	}

	source, err := g.wallet.Wallet(ctx, giver)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrWalletUnavailable, err)
	}
	target, err := g.wallet.Wallet(ctx, item.UserId)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrWalletUnavailable, err)
	}

	fee := g.fee.For(item.Amount)
	if g.feeWallet == nil {
		// Кошелёк для комиссии не задан: удерживать её некуда, а списать
		// «в никуда» значит нарушить сходимость средств в системе.
		fee = 0
	}
	if source.Available < item.Amount+fee {
		return fmt.Errorf("%w: available %s, required %s",
			ErrInsufficientFunds, source.Available, item.Amount+fee)
	}

	if err = g.wallet.Transfer(ctx, giver, TransferParams{
		IdempotencyKey: fmt.Sprintf("wishlist:%s:gift", item.Id),
		Source:         source.Id,
		Target:         target.Id,
		Value:          item.Amount,
		Message:        "Денежный подарок",
	}); err != nil {
		return fmt.Errorf("%w: %s", ErrWalletUnavailable, err)
	}

	if fee > 0 {
		if err = g.wallet.Transfer(ctx, giver, TransferParams{
			IdempotencyKey: fmt.Sprintf("wishlist:%s:fee", item.Id),
			Source:         source.Id,
			Target:         *g.feeWallet,
			Value:          fee,
			Message:        "Комиссия за денежный подарок",
		}); err != nil {
			// Подарок уже переведён. Отменять его из-за комиссии нельзя:
			// одаряемый получил средства, и откат отобрал бы их.
			slog.ErrorContext(ctx, "Gift transferred, fee is not charged",
				slog.String("item", item.Id.String()), slog.String("err", err.Error()))
		}
	}
	return nil
}

// ReleaseExpired возвращает в списки подарки с истёкшим резервом.
// Без этого брошенный резерв блокирует подарок навсегда.
func (g *Gifts) ReleaseExpired(ctx context.Context) error {
	released, err := g.db.ReleaseExpired(ctx)
	if err != nil {
		return err
	}
	if len(released) > 0 {
		slog.InfoContext(ctx, "Expired reservations released", slog.Int("count", len(released)))
	}
	return nil
}

func (g *Gifts) ownedItem(ctx context.Context, owner, id uuid.UUID) (wishlist.Item, error) {
	item, err := g.db.Item(ctx, id)
	if err != nil {
		return wishlist.Item{}, err
	}
	if item.UserId != owner {
		// Чужой элемент отдаётся как несуществующий: подтверждать его
		// наличие незачем.
		return wishlist.Item{}, ErrNotFound
	}
	return item, nil
}

// publish отправляет оповещение. Сбой оповещения не отменяет операцию:
// подарок остаётся зарезервированным, даже если сообщение не ушло.
func (g *Gifts) publish(
	ctx context.Context,
	user uuid.UUID,
	eventType notify.EventType,
	item wishlist.Item,
	dedupKey string,
) {
	if !g.notifier.Enabled() {
		return
	}
	if err := g.notifier.Publish(ctx, notify.PublishEvent{
		UserId:   user,
		Type:     eventType,
		Payload:  map[string]string{"item": item.Title},
		DedupKey: dedupKey,
	}); err != nil {
		slog.ErrorContext(ctx, "Can't publish notification",
			slog.String("event", string(eventType)),
			slog.String("item", item.Id.String()),
			slog.String("err", err.Error()))
	}
}
