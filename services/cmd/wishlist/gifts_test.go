package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"wish/services/shared/marketplace"
	"wish/services/shared/notify"
	"wish/services/shared/payment"
	"wish/services/shared/wishlist"

	"github.com/google/uuid"
)

// memoryDatabase — репозиторий в памяти. Реализован целиком, а не заглушкой
// на пару методов: операции ходят по нескольким методам подряд, и подмена
// одного из них проверяла бы не сценарий, а сам мок.
type memoryDatabase struct {
	mu    sync.Mutex
	items map[uuid.UUID]wishlist.Item
}

func newMemoryDatabase() *memoryDatabase {
	return &memoryDatabase{items: make(map[uuid.UUID]wishlist.Item)}
}

func (m *memoryDatabase) Create(_ context.Context, item wishlist.Item) (wishlist.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	item.Id = uuid.New()
	item.CreatedAt = time.Now()
	item.UpdatedAt = item.CreatedAt
	m.items[item.Id] = item
	return item, nil
}

func (m *memoryDatabase) Items(
	_ context.Context,
	owner uuid.UUID,
	states []wishlist.State,
) ([]wishlist.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	items := make([]wishlist.Item, 0, len(m.items))
	for _, item := range m.items {
		if item.UserId != owner {
			continue
		}
		if len(states) > 0 && !containsState(states, item.State) {
			continue
		}
		items = append(items, item)
	}
	return items, nil
}

func containsState(states []wishlist.State, state wishlist.State) bool {
	for _, candidate := range states {
		if candidate == state {
			return true
		}
	}
	return false
}

func (m *memoryDatabase) Chosen(_ context.Context, giver uuid.UUID) ([]wishlist.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	items := make([]wishlist.Item, 0)
	for _, item := range m.items {
		if item.GiverId != nil && *item.GiverId == giver {
			items = append(items, item)
		}
	}
	return items, nil
}

func (m *memoryDatabase) Item(_ context.Context, id uuid.UUID) (wishlist.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.items[id]
	if !ok {
		return wishlist.Item{}, ErrNotFound
	}
	return item, nil
}

func (m *memoryDatabase) Delete(_ context.Context, id, owner uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.items[id]
	if !ok || item.UserId != owner ||
		(item.State != wishlist.StateVisible && item.State != wishlist.StateHidden) {
		return ErrNotFound
	}
	delete(m.items, id)
	return nil
}

func (m *memoryDatabase) Transition(
	_ context.Context,
	id uuid.UUID,
	transition Transition,
) (wishlist.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	item, ok := m.items[id]
	if !ok {
		return wishlist.Item{}, ErrNotFound
	}
	if err := wishlist.CanTransition(item.State, transition.To, transition.Actor); err != nil {
		return wishlist.Item{}, err
	}
	if transition.Actor == wishlist.ActorGiver && item.GiverId != nil &&
		transition.Giver != nil && *item.GiverId != *transition.Giver {
		return wishlist.Item{}, wishlist.ErrForbiddenTransition
	}

	item.State = transition.To
	switch transition.To {
	case wishlist.StateVisible, wishlist.StateHidden:
		item.GiverId = nil
		item.ReservedUntil = nil
	default:
		if transition.Giver != nil {
			item.GiverId = transition.Giver
		}
		item.ReservedUntil = transition.ReservedUntil
	}
	if transition.OrderId != "" {
		item.OrderId = transition.OrderId
	}
	item.UpdatedAt = time.Now()
	m.items[id] = item
	return item, nil
}

func (m *memoryDatabase) ReleaseExpired(context.Context) ([]wishlist.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	released := make([]wishlist.Item, 0)
	for id, item := range m.items {
		if item.State != wishlist.StateChosen || item.ReservedUntil == nil ||
			item.ReservedUntil.After(time.Now()) {
			continue
		}
		item.State = wishlist.StateVisible
		item.GiverId = nil
		item.ReservedUntil = nil
		m.items[id] = item
		released = append(released, item)
	}
	return released, nil
}

func (m *memoryDatabase) Close() error               { return nil }
func (m *memoryDatabase) Stats() sql.DBStats         { return sql.DBStats{} }
func (m *memoryDatabase) Ping(context.Context) error { return nil }

// fakeWallet подменяет сервис кошелька.
type fakeWallet struct {
	mu        sync.Mutex
	wallets   map[uuid.UUID]WalletInfo
	transfers []TransferParams
	failFee   bool
}

func (f *fakeWallet) Wallet(_ context.Context, user uuid.UUID) (WalletInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info, ok := f.wallets[user]
	if !ok {
		return WalletInfo{}, errors.New("нет кошелька")
	}
	return info, nil
}

func (f *fakeWallet) Transfer(_ context.Context, _ uuid.UUID, params TransferParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failFee && params.Message == "Комиссия за денежный подарок" {
		return errors.New("кошелёк недоступен")
	}
	f.transfers = append(f.transfers, params)
	return nil
}

// notifyStub принимает оповещения вместо сервиса notify.
type notifyStub struct {
	mu     sync.Mutex
	events []notify.PublishEvent
}

func (n *notifyStub) start(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event notify.PublishEvent
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		n.mu.Lock()
		n.events = append(n.events, event)
		n.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func (n *notifyStub) received() []notify.PublishEvent {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]notify.PublishEvent(nil), n.events...)
}

type testEnvironment struct {
	gifts  *Gifts
	db     *memoryDatabase
	wallet *fakeWallet
	events *notifyStub
	stub   *marketplace.Stub
}

func newTestEnvironment(t *testing.T, fee payment.Fee, feeWallet *uuid.UUID) *testEnvironment {
	t.Helper()

	events := &notifyStub{}
	stub := &marketplace.Stub{}
	db := newMemoryDatabase()
	wallet := &fakeWallet{wallets: make(map[uuid.UUID]WalletInfo)}

	gifts := NewGifts(db, marketplace.NewRegistry(stub),
		notify.NewClient(events.start(t), uuid.New()), wallet, fee, feeWallet, time.Hour)
	return &testEnvironment{gifts: gifts, db: db, wallet: wallet, events: events, stub: stub}
}

func (e *testEnvironment) addProduct(t *testing.T, owner uuid.UUID) wishlist.Item {
	t.Helper()
	item, err := e.gifts.Add(context.Background(), owner, wishlist.CreateItem{
		Kind: wishlist.KindProduct, Priority: 1,
		Provider: marketplace.ProviderStub, ProductId: "coffee-machine",
	})
	if err != nil {
		t.Fatalf("добавление товара: %v", err)
	}
	return item
}

func TestAddProductTakesPriceFromMarketplace(t *testing.T) {
	env := newTestEnvironment(t, payment.Fee{}, nil)
	owner := uuid.New()

	item := env.addProduct(t, owner)
	if item.Price <= 0 {
		t.Errorf("цена не взята из карточки: %s", item.Price)
	}
	if item.Title == "" || item.URL == "" {
		t.Errorf("карточка не перенесена в элемент: %+v", item)
	}
	if item.PriceAt == nil {
		t.Error("нет отметки времени цены: цена на площадке меняется")
	}
	if item.State != wishlist.StateVisible {
		t.Errorf("состояние %s, ожидалось %s", item.State, wishlist.StateVisible)
	}
}

func TestAddProductWhenMarketplaceUnavailable(t *testing.T) {
	env := newTestEnvironment(t, payment.Fee{}, nil)
	env.stub.Unavailable = true

	// Подставлять вместо цены ноль нельзя: элемент попал бы в расчёты
	// как бесплатный.
	_, err := env.gifts.Add(context.Background(), uuid.New(), wishlist.CreateItem{
		Kind: wishlist.KindProduct, Priority: 1,
		Provider: marketplace.ProviderStub, ProductId: "coffee-machine",
	})
	if !errors.Is(err, ErrMarketplaceUnavailable) {
		t.Errorf("получено %v, ожидалась %v", err, ErrMarketplaceUnavailable)
	}
}

func TestForeignListShowsOnlyVisible(t *testing.T) {
	ctx := context.Background()
	env := newTestEnvironment(t, payment.Fee{}, nil)
	owner := uuid.New()
	giver := uuid.New()

	visible := env.addProduct(t, owner)
	hidden := env.addProduct(t, owner)
	if _, err := env.gifts.Hide(ctx, owner, hidden.Id); err != nil {
		t.Fatalf("скрытие элемента: %v", err)
	}

	items, err := env.gifts.List(ctx, giver, owner)
	if err != nil {
		t.Fatalf("чтение чужого списка: %v", err)
	}
	if len(items) != 1 || items[0].Id != visible.Id {
		t.Fatalf("чужому видно %d элементов, ожидался один видимый", len(items))
	}

	own, err := env.gifts.List(ctx, owner, owner)
	if err != nil {
		t.Fatalf("чтение своего списка: %v", err)
	}
	if len(own) != 2 {
		t.Errorf("владелец видит %d элементов, ожидалось 2", len(own))
	}
}

func TestReserveNotifiesOwnerWithoutGiverName(t *testing.T) {
	ctx := context.Background()
	env := newTestEnvironment(t, payment.Fee{}, nil)
	owner := uuid.New()
	giver := uuid.New()
	item := env.addProduct(t, owner)

	reserved, err := env.gifts.Reserve(ctx, giver, item.Id)
	if err != nil {
		t.Fatalf("резервирование: %v", err)
	}
	if reserved.State != wishlist.StateChosen {
		t.Errorf("состояние %s, ожидалось %s", reserved.State, wishlist.StateChosen)
	}
	if reserved.ReservedUntil == nil {
		t.Error("резерв без срока: брошенный резерв заблокирует подарок навсегда")
	}

	events := env.events.received()
	if len(events) != 1 {
		t.Fatalf("оповещений %d, ожидалось 1", len(events))
	}
	if events[0].UserId != owner || events[0].Type != notify.EventWishlistItemReserved {
		t.Errorf("оповещение: %+v", events[0])
	}
	// Имя дарителя в оповещение не попадает: сюрприз входит в продукт.
	if _, ok := events[0].Payload["giver"]; ok {
		t.Error("оповещение раскрывает дарителя")
	}
}

func TestReserveOwnItemForbidden(t *testing.T) {
	env := newTestEnvironment(t, payment.Fee{}, nil)
	owner := uuid.New()
	item := env.addProduct(t, owner)

	if _, err := env.gifts.Reserve(context.Background(), owner, item.Id); !errors.Is(err, ErrForbidden) {
		t.Errorf("получено %v, ожидалась %v", err, ErrForbidden)
	}
}

func TestSecondGiverCannotReserve(t *testing.T) {
	ctx := context.Background()
	env := newTestEnvironment(t, payment.Fee{}, nil)
	owner := uuid.New()
	item := env.addProduct(t, owner)

	if _, err := env.gifts.Reserve(ctx, uuid.New(), item.Id); err != nil {
		t.Fatalf("первое резервирование: %v", err)
	}
	// Выбранный подарок не виден другим дарителям — он для них
	// не существует.
	if _, err := env.gifts.Reserve(ctx, uuid.New(), item.Id); !errors.Is(err, ErrNotFound) {
		t.Errorf("получено %v, ожидалась %v", err, ErrNotFound)
	}
}

func TestConfirmAndRejectNotifyGiver(t *testing.T) {
	tests := []struct {
		name  string
		act   func(*Gifts, context.Context, uuid.UUID, uuid.UUID) (wishlist.Item, error)
		state wishlist.State
		event notify.EventType
	}{
		{"подтверждение", (*Gifts).Confirm, wishlist.StateConfirmed, notify.EventWishlistItemConfirmed},
		{"отклонение", (*Gifts).Reject, wishlist.StateRejected, notify.EventWishlistItemRejected},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			env := newTestEnvironment(t, payment.Fee{}, nil)
			owner := uuid.New()
			giver := uuid.New()
			item := env.addProduct(t, owner)
			if _, err := env.gifts.Reserve(ctx, giver, item.Id); err != nil {
				t.Fatalf("резервирование: %v", err)
			}

			decided, err := test.act(env.gifts, ctx, owner, item.Id)
			if err != nil {
				t.Fatalf("решение одаряемого: %v", err)
			}
			if decided.State != test.state {
				t.Errorf("состояние %s, ожидалось %s", decided.State, test.state)
			}

			events := env.events.received()
			if len(events) != 2 {
				t.Fatalf("оповещений %d, ожидалось 2", len(events))
			}
			if events[1].UserId != giver || events[1].Type != test.event {
				t.Errorf("оповещение дарителю: %+v", events[1])
			}
		})
	}
}

func TestStrangerCannotDecide(t *testing.T) {
	ctx := context.Background()
	env := newTestEnvironment(t, payment.Fee{}, nil)
	owner := uuid.New()
	giver := uuid.New()
	item := env.addProduct(t, owner)
	if _, err := env.gifts.Reserve(ctx, giver, item.Id); err != nil {
		t.Fatalf("резервирование: %v", err)
	}

	// Даритель не решает за одаряемого, и посторонний тем более.
	if _, err := env.gifts.Confirm(ctx, giver, item.Id); !errors.Is(err, ErrNotFound) {
		t.Errorf("даритель подтвердил за одаряемого: %v", err)
	}
	if _, err := env.gifts.Reject(ctx, uuid.New(), item.Id); !errors.Is(err, ErrNotFound) {
		t.Errorf("посторонний отклонил подарок: %v", err)
	}
}

func TestAcceptProductWithoutMarketplaceOrder(t *testing.T) {
	ctx := context.Background()
	env := newTestEnvironment(t, payment.Fee{}, nil)
	owner := uuid.New()
	giver := uuid.New()
	item := env.addProduct(t, owner)

	if _, err := env.gifts.Reserve(ctx, giver, item.Id); err != nil {
		t.Fatalf("резервирование: %v", err)
	}
	if _, err := env.gifts.Confirm(ctx, owner, item.Id); err != nil {
		t.Fatalf("подтверждение: %v", err)
	}

	accepted, err := env.gifts.Accept(ctx, giver, item.Id)
	if err != nil {
		t.Fatalf("акцепт: %v", err)
	}
	if accepted.State != wishlist.StateAccepted {
		t.Errorf("состояние %s, ожидалось %s", accepted.State, wishlist.StateAccepted)
	}
	// Площадка не поддерживает оформление заказа покупателем (ADR 0004):
	// это не ошибка, подарок заказывается дарителем вручную.
	if accepted.OrderId != "" {
		t.Errorf("номер заказа %q, ожидался пустой", accepted.OrderId)
	}

	events := env.events.received()
	if last := events[len(events)-1]; last.Type != notify.EventWishlistItemGifted || last.UserId != owner {
		t.Errorf("итоговое оповещение: %+v", last)
	}
}

func TestAcceptProductWithMarketplaceOrder(t *testing.T) {
	ctx := context.Background()
	env := newTestEnvironment(t, payment.Fee{}, nil)
	env.stub.OrderSupported = true
	owner := uuid.New()
	giver := uuid.New()
	item := env.addProduct(t, owner)

	if _, err := env.gifts.Reserve(ctx, giver, item.Id); err != nil {
		t.Fatalf("резервирование: %v", err)
	}
	if _, err := env.gifts.Confirm(ctx, owner, item.Id); err != nil {
		t.Fatalf("подтверждение: %v", err)
	}
	accepted, err := env.gifts.Accept(ctx, giver, item.Id)
	if err != nil {
		t.Fatalf("акцепт: %v", err)
	}
	if accepted.OrderId == "" {
		t.Error("номер заказа не сохранён")
	}
}

func TestAcceptByAnotherGiverForbidden(t *testing.T) {
	ctx := context.Background()
	env := newTestEnvironment(t, payment.Fee{}, nil)
	owner := uuid.New()
	giver := uuid.New()
	item := env.addProduct(t, owner)

	if _, err := env.gifts.Reserve(ctx, giver, item.Id); err != nil {
		t.Fatalf("резервирование: %v", err)
	}
	if _, err := env.gifts.Confirm(ctx, owner, item.Id); err != nil {
		t.Fatalf("подтверждение: %v", err)
	}
	if _, err := env.gifts.Accept(ctx, uuid.New(), item.Id); !errors.Is(err, ErrForbidden) {
		t.Errorf("чужой даритель акцептовал подарок: %v", err)
	}
}

func TestAcceptMoneyTransfersWithFee(t *testing.T) {
	ctx := context.Background()
	feeWallet := uuid.New()
	env := newTestEnvironment(t, payment.Fee{BasisPoints: 250}, &feeWallet)
	owner := uuid.New()
	giver := uuid.New()

	giverWallet := uuid.New()
	ownerWallet := uuid.New()
	env.wallet.wallets[giver] = WalletInfo{Id: giverWallet, Available: 10_000_00}
	env.wallet.wallets[owner] = WalletInfo{Id: ownerWallet, Available: 0}

	item, err := env.gifts.Add(ctx, owner, wishlist.CreateItem{
		Kind: wishlist.KindMoney, Priority: 1, Amount: 1_000_00, Title: "На велосипед",
	})
	if err != nil {
		t.Fatalf("добавление денежного элемента: %v", err)
	}
	if _, err = env.gifts.Reserve(ctx, giver, item.Id); err != nil {
		t.Fatalf("резервирование: %v", err)
	}
	if _, err = env.gifts.Confirm(ctx, owner, item.Id); err != nil {
		t.Fatalf("подтверждение: %v", err)
	}
	if _, err = env.gifts.Accept(ctx, giver, item.Id); err != nil {
		t.Fatalf("акцепт: %v", err)
	}

	if len(env.wallet.transfers) != 2 {
		t.Fatalf("переводов %d, ожидалось 2 (подарок и комиссия)", len(env.wallet.transfers))
	}
	gift, fee := env.wallet.transfers[0], env.wallet.transfers[1]
	if gift.Target != ownerWallet || gift.Value != 1_000_00 {
		t.Errorf("подарок: %+v", gift)
	}
	if fee.Target != feeWallet || fee.Value != 25_00 {
		t.Errorf("комиссия: %+v", fee)
	}
	// Ключи идемпотентности выведены из элемента: повтор акцепта
	// не спишет средства второй раз.
	if gift.IdempotencyKey == fee.IdempotencyKey {
		t.Error("подарок и комиссия идут с одним ключом идемпотентности")
	}
}

func TestAcceptMoneyChecksFunds(t *testing.T) {
	ctx := context.Background()
	feeWallet := uuid.New()
	env := newTestEnvironment(t, payment.Fee{BasisPoints: 250}, &feeWallet)
	owner := uuid.New()
	giver := uuid.New()

	// Средств хватает на подарок, но не на подарок вместе с комиссией.
	env.wallet.wallets[giver] = WalletInfo{Id: uuid.New(), Available: 1_000_00}
	env.wallet.wallets[owner] = WalletInfo{Id: uuid.New()}

	item, err := env.gifts.Add(ctx, owner, wishlist.CreateItem{
		Kind: wishlist.KindMoney, Priority: 1, Amount: 1_000_00, Title: "На велосипед",
	})
	if err != nil {
		t.Fatalf("добавление денежного элемента: %v", err)
	}
	if _, err = env.gifts.Reserve(ctx, giver, item.Id); err != nil {
		t.Fatalf("резервирование: %v", err)
	}
	if _, err = env.gifts.Confirm(ctx, owner, item.Id); err != nil {
		t.Fatalf("подтверждение: %v", err)
	}

	if _, err = env.gifts.Accept(ctx, giver, item.Id); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("получено %v, ожидалась %v", err, ErrInsufficientFunds)
	}
	if len(env.wallet.transfers) != 0 {
		t.Errorf("переводы при нехватке средств: %+v", env.wallet.transfers)
	}
	// Подарок остаётся подтверждённым: даритель пополнит кошелёк
	// и повторит акцепт.
	current, err := env.db.Item(ctx, item.Id)
	if err != nil {
		t.Fatalf("чтение элемента: %v", err)
	}
	if current.State != wishlist.StateConfirmed {
		t.Errorf("состояние %s, ожидалось %s", current.State, wishlist.StateConfirmed)
	}
}

// TestAcceptMoneyKeepsGiftWhenFeeFails фиксирует выбранный порядок:
// сначала подарок, затем комиссия. Не удержанная комиссия — потеря
// системы, а не пользователя, и отменять из-за неё вручённый подарок нельзя.
func TestAcceptMoneyKeepsGiftWhenFeeFails(t *testing.T) {
	ctx := context.Background()
	feeWallet := uuid.New()
	env := newTestEnvironment(t, payment.Fee{BasisPoints: 250}, &feeWallet)
	env.wallet.failFee = true
	owner := uuid.New()
	giver := uuid.New()
	env.wallet.wallets[giver] = WalletInfo{Id: uuid.New(), Available: 10_000_00}
	env.wallet.wallets[owner] = WalletInfo{Id: uuid.New()}

	item, err := env.gifts.Add(ctx, owner, wishlist.CreateItem{
		Kind: wishlist.KindMoney, Priority: 1, Amount: 1_000_00, Title: "На велосипед",
	})
	if err != nil {
		t.Fatalf("добавление денежного элемента: %v", err)
	}
	if _, err = env.gifts.Reserve(ctx, giver, item.Id); err != nil {
		t.Fatalf("резервирование: %v", err)
	}
	if _, err = env.gifts.Confirm(ctx, owner, item.Id); err != nil {
		t.Fatalf("подтверждение: %v", err)
	}

	accepted, err := env.gifts.Accept(ctx, giver, item.Id)
	if err != nil {
		t.Fatalf("акцепт: %v", err)
	}
	if accepted.State != wishlist.StateAccepted {
		t.Errorf("состояние %s, ожидалось %s", accepted.State, wishlist.StateAccepted)
	}
	if len(env.wallet.transfers) != 1 {
		t.Errorf("переводов %d, ожидался один — подарок", len(env.wallet.transfers))
	}
}

func TestReleaseExpiredReturnsItemToList(t *testing.T) {
	ctx := context.Background()
	env := newTestEnvironment(t, payment.Fee{}, nil)
	owner := uuid.New()
	giver := uuid.New()
	item := env.addProduct(t, owner)

	if _, err := env.gifts.Reserve(ctx, giver, item.Id); err != nil {
		t.Fatalf("резервирование: %v", err)
	}
	// Срок резерва вышел.
	expired := time.Now().Add(-time.Minute)
	reserved, _ := env.db.Item(ctx, item.Id)
	reserved.ReservedUntil = &expired
	env.db.items[item.Id] = reserved

	if err := env.gifts.ReleaseExpired(ctx); err != nil {
		t.Fatalf("освобождение резервов: %v", err)
	}
	current, err := env.db.Item(ctx, item.Id)
	if err != nil {
		t.Fatalf("чтение элемента: %v", err)
	}
	if current.State != wishlist.StateVisible || current.GiverId != nil {
		t.Errorf("просроченный резерв не снят: %+v", current)
	}
}

func TestNotificationFailureDoesNotBreakOperation(t *testing.T) {
	ctx := context.Background()
	env := newTestEnvironment(t, payment.Fee{}, nil)
	owner := uuid.New()
	item := env.addProduct(t, owner)

	// Сервис оповещений недоступен: адрес указывает в никуда.
	env.gifts.notifier = notify.NewClient("http://127.0.0.1:1", uuid.New())

	if _, err := env.gifts.Reserve(ctx, uuid.New(), item.Id); err != nil {
		t.Fatalf("резервирование сорвалось из-за оповещения: %v", err)
	}
	current, _ := env.db.Item(ctx, item.Id)
	if current.State != wishlist.StateChosen {
		t.Errorf("состояние %s, ожидалось %s", current.State, wishlist.StateChosen)
	}
}
