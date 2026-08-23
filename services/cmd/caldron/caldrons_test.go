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

	"wish/services/shared/caldron"
	"wish/services/shared/credit"
	"wish/services/shared/marketplace"
	"wish/services/shared/notify"
	"wish/services/shared/wallets"

	"github.com/google/uuid"
)

// memoryDatabase — репозиторий в памяти. Реализован целиком: операции
// котла ходят по нескольким методам подряд, и подмена одного из них
// проверяла бы мок, а не сценарий.
type memoryDatabase struct {
	mu           sync.Mutex
	caldrons     map[uuid.UUID]caldron.Caldron
	participants map[uuid.UUID][]caldron.Participant
	gifts        map[uuid.UUID][]caldron.Gift
	seeds        map[uuid.UUID][]byte
	draws        map[uuid.UUID]caldron.Draw
}

func newMemoryDatabase() *memoryDatabase {
	return &memoryDatabase{
		caldrons:     make(map[uuid.UUID]caldron.Caldron),
		participants: make(map[uuid.UUID][]caldron.Participant),
		gifts:        make(map[uuid.UUID][]caldron.Gift),
		seeds:        make(map[uuid.UUID][]byte),
		draws:        make(map[uuid.UUID]caldron.Draw),
	}
}

func (m *memoryDatabase) SetArbiter(
	_ context.Context,
	id uuid.UUID,
	arbiter *uuid.UUID,
) (caldron.Caldron, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	pot, ok := m.caldrons[id]
	if !ok {
		return caldron.Caldron{}, ErrNotFound
	}
	if pot.State.Terminal() {
		return caldron.Caldron{}, caldron.ErrInvalidTransition
	}
	pot.ArbiterId = arbiter
	m.caldrons[id] = pot
	return m.build(id)
}

func (m *memoryDatabase) ReplaceGifts(
	_ context.Context,
	id, user uuid.UUID,
	gifts []caldron.Gift,
) ([]caldron.Gift, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	kept := make([]caldron.Gift, 0, len(m.gifts[id]))
	for _, gift := range m.gifts[id] {
		if gift.UserId != user {
			kept = append(kept, gift)
		}
	}
	m.gifts[id] = append(kept, gifts...)
	return m.giftsOf(id, &user), nil
}

func (m *memoryDatabase) Gifts(_ context.Context, id uuid.UUID, user *uuid.UUID) ([]caldron.Gift, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.giftsOf(id, user), nil
}

// giftsOf вызывается под мьютексом.
func (m *memoryDatabase) giftsOf(id uuid.UUID, user *uuid.UUID) []caldron.Gift {
	found := make([]caldron.Gift, 0, len(m.gifts[id]))
	for _, gift := range m.gifts[id] {
		if user == nil || gift.UserId == *user {
			found = append(found, gift)
		}
	}
	return found
}

func (m *memoryDatabase) Seed(_ context.Context, id uuid.UUID) ([]byte, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	seed, ok := m.seeds[id]
	if !ok {
		return nil, "", ErrNotFound
	}
	return seed, caldron.Commit(seed), nil
}

func (m *memoryDatabase) SaveDraw(_ context.Context, draw caldron.Draw) (caldron.Draw, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Розыгрыш бывает один: повтор возвращает уже сохранённый результат.
	if existing, ok := m.draws[draw.CaldronId]; ok {
		return existing, nil
	}
	draw.CreatedAt = time.Now()
	m.draws[draw.CaldronId] = draw
	return draw, nil
}

func (m *memoryDatabase) Draw(_ context.Context, id uuid.UUID) (caldron.Draw, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	draw, ok := m.draws[id]
	if !ok {
		return caldron.Draw{}, ErrNoDraw
	}
	return draw, nil
}

func (m *memoryDatabase) Create(_ context.Context, create caldron.Caldron) (caldron.Caldron, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	seed, err := caldron.NewSeed()
	if err != nil {
		return caldron.Caldron{}, err
	}

	create.Id = uuid.New()
	create.State = caldron.StatePreparing
	create.Commitment = caldron.Commit(seed)
	create.CreatedAt = time.Now()
	create.UpdatedAt = create.CreatedAt
	m.caldrons[create.Id] = create
	m.seeds[create.Id] = seed
	return create, nil
}

// build собирает котёл с участниками. Вызывается под мьютексом.
func (m *memoryDatabase) build(id uuid.UUID) (caldron.Caldron, error) {
	pot, ok := m.caldrons[id]
	if !ok {
		return caldron.Caldron{}, ErrNotFound
	}
	pot.Participants = append([]caldron.Participant(nil), m.participants[id]...)
	pot.Collected = 0
	for _, participant := range pot.Participants {
		if participant.State == caldron.ParticipantPaid {
			pot.Collected += participant.Contributed
		}
	}
	return pot, nil
}

func (m *memoryDatabase) Caldron(_ context.Context, id uuid.UUID) (caldron.Caldron, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.build(id)
}

func (m *memoryDatabase) ByUser(_ context.Context, user uuid.UUID) ([]caldron.Caldron, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	caldrons := make([]caldron.Caldron, 0)
	for id, pot := range m.caldrons {
		built, err := m.build(id)
		if err != nil {
			return nil, err
		}
		if pot.CreatorId == user || built.IsParticipant(user) {
			caldrons = append(caldrons, built)
		}
	}
	return caldrons, nil
}

func (m *memoryDatabase) AddParticipant(
	_ context.Context,
	id uuid.UUID,
	add caldron.AddParticipant,
) (caldron.Caldron, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	pot, ok := m.caldrons[id]
	if !ok {
		return caldron.Caldron{}, ErrNotFound
	}
	if pot.State != caldron.StatePreparing {
		return caldron.Caldron{}, caldron.ErrInvalidTransition
	}

	expected := add.Amount
	if pot.Mode == caldron.ModeFixed {
		expected = pot.Amount
	}
	for _, participant := range m.participants[id] {
		if participant.UserId == add.UserId {
			return m.build(id)
		}
	}
	m.participants[id] = append(m.participants[id], caldron.Participant{
		CaldronId: id, UserId: add.UserId, Expected: expected,
		State: caldron.ParticipantInvited, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	return m.build(id)
}

func (m *memoryDatabase) RemoveParticipant(_ context.Context, id, user uuid.UUID) (caldron.Caldron, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	pot, ok := m.caldrons[id]
	if !ok {
		return caldron.Caldron{}, ErrNotFound
	}
	if pot.State != caldron.StatePreparing {
		return caldron.Caldron{}, caldron.ErrInvalidTransition
	}

	kept := make([]caldron.Participant, 0, len(m.participants[id]))
	removed := false
	for _, participant := range m.participants[id] {
		if participant.UserId == user && participant.State == caldron.ParticipantInvited {
			removed = true
			continue
		}
		kept = append(kept, participant)
	}
	if !removed {
		return caldron.Caldron{}, ErrParticipantNotFound
	}
	m.participants[id] = kept
	return m.build(id)
}

func (m *memoryDatabase) SetWallet(_ context.Context, id, wallet uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	pot, ok := m.caldrons[id]
	if !ok {
		return ErrNotFound
	}
	if pot.WalletId == nil {
		pot.WalletId = &wallet
		m.caldrons[id] = pot
	}
	return nil
}

func (m *memoryDatabase) StartContribution(
	_ context.Context,
	id, user uuid.UUID,
	requested credit.Amount,
) (caldron.Caldron, credit.Amount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	pot, err := m.build(id)
	if err != nil {
		return caldron.Caldron{}, 0, err
	}

	for _, participant := range pot.Participants {
		if participant.UserId != user {
			continue
		}
		if participant.State != caldron.ParticipantInvited {
			return caldron.Caldron{}, 0, ErrAlreadyPaid
		}
		if pot.State != caldron.StatePreparing {
			return caldron.Caldron{}, 0, caldron.ErrInvalidTransition
		}
		amount, err := pot.ContributionFor(participant, requested)
		if err != nil {
			return caldron.Caldron{}, 0, err
		}
		return pot, amount, nil
	}
	return caldron.Caldron{}, 0, ErrParticipantNotFound
}

func (m *memoryDatabase) MarkPaid(
	_ context.Context,
	id, user uuid.UUID,
	amount credit.Amount,
) (caldron.Caldron, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i, participant := range m.participants[id] {
		if participant.UserId == user && participant.State == caldron.ParticipantInvited {
			m.participants[id][i].State = caldron.ParticipantPaid
			m.participants[id][i].Contributed = amount
		}
	}

	pot, err := m.build(id)
	if err != nil {
		return caldron.Caldron{}, err
	}
	if pot.State == caldron.StatePreparing && pot.Complete() {
		stored := m.caldrons[id]
		stored.State = caldron.StateReady
		m.caldrons[id] = stored
		return m.build(id)
	}
	return pot, nil
}

func (m *memoryDatabase) MarkRefunded(_ context.Context, id, user uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i, participant := range m.participants[id] {
		if participant.UserId == user && participant.State == caldron.ParticipantPaid {
			m.participants[id][i].State = caldron.ParticipantRefunded
		}
	}
	return nil
}

func (m *memoryDatabase) Transition(
	_ context.Context,
	id uuid.UUID,
	to caldron.State,
	actor caldron.Actor,
) (caldron.Caldron, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	pot, ok := m.caldrons[id]
	if !ok {
		return caldron.Caldron{}, ErrNotFound
	}
	if err := caldron.CanTransition(pot.State, to, actor); err != nil {
		return caldron.Caldron{}, err
	}
	pot.State = to
	pot.UpdatedAt = time.Now()
	m.caldrons[id] = pot
	return m.build(id)
}

func (m *memoryDatabase) PendingRefunds(_ context.Context, limit int) ([]caldron.Caldron, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	pending := make([]caldron.Caldron, 0)
	for id, pot := range m.caldrons {
		if pot.State != caldron.StateCancelled || len(pending) >= limit {
			continue
		}
		built, err := m.build(id)
		if err != nil {
			return nil, err
		}
		for _, participant := range built.Participants {
			if participant.State == caldron.ParticipantPaid {
				pending = append(pending, built)
				break
			}
		}
	}
	return pending, nil
}

func (m *memoryDatabase) Close() error               { return nil }
func (m *memoryDatabase) Stats() sql.DBStats         { return sql.DBStats{} }
func (m *memoryDatabase) Ping(context.Context) error { return nil }

// fakeWallet ведёт балансы кошельков и, как настоящий кошелёк, отсекает
// повтор по ключу идемпотентности: без этого проверять идемпотентность
// возврата было бы не на чем.
type fakeWallet struct {
	mu       sync.Mutex
	owners   map[uuid.UUID]uuid.UUID
	balances map[uuid.UUID]credit.Amount
	applied  map[string]bool
	fail     map[uuid.UUID]bool
	calls    int
}

func newFakeWallet() *fakeWallet {
	return &fakeWallet{
		owners:   make(map[uuid.UUID]uuid.UUID),
		balances: make(map[uuid.UUID]credit.Amount),
		applied:  make(map[string]bool),
		fail:     make(map[uuid.UUID]bool),
	}
}

func (f *fakeWallet) fund(owner uuid.UUID, amount credit.Amount) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()

	wallet := uuid.New()
	f.owners[owner] = wallet
	f.balances[wallet] = amount
	return wallet
}

func (f *fakeWallet) Wallet(_ context.Context, owner uuid.UUID) (wallets.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.fail[owner] {
		return wallets.Info{}, errors.New("кошелёк недоступен")
	}
	wallet, ok := f.owners[owner]
	if !ok {
		// Кошелёк заводится при первом обращении — как в настоящем сервисе.
		wallet = uuid.New()
		f.owners[owner] = wallet
	}
	return wallets.Info{Id: wallet, Balance: f.balances[wallet], Available: f.balances[wallet]}, nil
}

func (f *fakeWallet) Transfer(_ context.Context, owner uuid.UUID, params wallets.TransferParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++
	if f.fail[owner] {
		return errors.New("кошелёк недоступен")
	}
	if f.applied[params.IdempotencyKey] {
		// Повтор с тем же ключом возвращает результат первой операции
		// и денег не двигает.
		return nil
	}
	if f.balances[params.Source] < params.Value {
		return errors.New("недостаточно средств")
	}
	f.balances[params.Source] -= params.Value
	f.balances[params.Target] += params.Value
	f.applied[params.IdempotencyKey] = true
	return nil
}

func (f *fakeWallet) total() credit.Amount {
	f.mu.Lock()
	defer f.mu.Unlock()

	var total credit.Amount
	for _, balance := range f.balances {
		total += balance
	}
	return total
}

func (f *fakeWallet) balanceOf(owner uuid.UUID) credit.Amount {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.balances[f.owners[owner]]
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

func (n *notifyStub) byType(eventType notify.EventType) []notify.PublishEvent {
	n.mu.Lock()
	defer n.mu.Unlock()

	found := make([]notify.PublishEvent, 0)
	for _, event := range n.events {
		if event.Type == eventType {
			found = append(found, event)
		}
	}
	return found
}

type environment struct {
	caldrons *Caldrons
	db       *memoryDatabase
	wallet   *fakeWallet
	events   *notifyStub
}

func newEnvironment(t *testing.T) *environment {
	t.Helper()

	events := &notifyStub{}
	db := newMemoryDatabase()
	wallet := newFakeWallet()
	return &environment{
		caldrons: NewCaldrons(db, wallet, notify.NewClient(events.start(t), uuid.New()),
			marketplace.NewRegistry(&marketplace.Stub{})),
		db:     db,
		wallet: wallet,
		events: events,
	}
}

// fixedCaldron собирает котёл с точной суммой взноса и участниками.
func (e *environment) fixedCaldron(
	t *testing.T,
	creator uuid.UUID,
	amount credit.Amount,
	members ...uuid.UUID,
) caldron.Caldron {
	t.Helper()
	ctx := context.Background()

	pot, err := e.caldrons.Create(ctx, creator, caldron.CreateCaldron{
		Title: "Юбилей", Type: caldron.TypeGift, Mode: caldron.ModeFixed,
		CreatorParticipates: true, Amount: amount,
	})
	if err != nil {
		t.Fatalf("создание котла: %v", err)
	}
	for _, member := range members {
		if pot, err = e.caldrons.AddParticipant(ctx, creator, pot.Id,
			caldron.AddParticipant{UserId: member}); err != nil {
			t.Fatalf("добавление участника: %v", err)
		}
	}
	return pot
}

func TestCreatorParticipatesOrArbitrates(t *testing.T) {
	ctx := context.Background()
	env := newEnvironment(t)
	creator := uuid.New()

	participating, err := env.caldrons.Create(ctx, creator, caldron.CreateCaldron{
		Title: "Юбилей", Type: caldron.TypeGift, Mode: caldron.ModeFixed,
		CreatorParticipates: true, Amount: 1_000_00,
	})
	if err != nil {
		t.Fatalf("создание котла: %v", err)
	}
	if !participating.IsParticipant(creator) {
		t.Error("создатель-участник не попал в список участников")
	}

	arbiter, err := env.caldrons.Create(ctx, creator, caldron.CreateCaldron{
		Title: "Юбилей", Type: caldron.TypeLuck, Mode: caldron.ModeFixed,
		CreatorParticipates: false, Amount: 1_000_00,
	})
	if err != nil {
		t.Fatalf("создание котла с арбитром: %v", err)
	}
	// Арбитр организует сбор, но не скидывается: ждать от него взнос
	// значит никогда не собрать котёл.
	if arbiter.IsParticipant(creator) {
		t.Error("создатель-арбитр попал в список участников")
	}
}

func TestOnlyCreatorManagesParticipants(t *testing.T) {
	ctx := context.Background()
	env := newEnvironment(t)
	creator := uuid.New()
	member := uuid.New()
	pot := env.fixedCaldron(t, creator, 1_000_00, member)

	t.Run("участник не добавляет других", func(t *testing.T) {
		_, err := env.caldrons.AddParticipant(ctx, member, pot.Id,
			caldron.AddParticipant{UserId: uuid.New()})
		if !errors.Is(err, ErrForbidden) {
			t.Errorf("получено %v, ожидалась %v", err, ErrForbidden)
		}
	})

	t.Run("посторонний не видит котла", func(t *testing.T) {
		if _, err := env.caldrons.Caldron(ctx, uuid.New(), pot.Id); !errors.Is(err, ErrNotFound) {
			t.Errorf("получено %v, ожидалась %v", err, ErrNotFound)
		}
	})

	t.Run("участник получил оповещение о добавлении", func(t *testing.T) {
		events := env.events.byType(notify.EventCaldronMemberAdded)
		if len(events) != 1 || events[0].UserId != member {
			t.Errorf("оповещения о добавлении: %+v", events)
		}
		if events[0].Payload["amount"] == "" {
			t.Error("в оповещении не сказано, сколько ждут")
		}
	})
}

func TestCaldronBecomesReadyWhenEveryoneContributed(t *testing.T) {
	ctx := context.Background()
	env := newEnvironment(t)
	creator := uuid.New()
	member := uuid.New()
	env.wallet.fund(creator, 10_000_00)
	env.wallet.fund(member, 10_000_00)

	pot := env.fixedCaldron(t, creator, 2_500_00, member)

	after, err := env.caldrons.Contribute(ctx, creator, pot.Id, 0)
	if err != nil {
		t.Fatalf("взнос создателя: %v", err)
	}
	// Внесли не все: котёл ещё собирается.
	if after.State != caldron.StatePreparing {
		t.Fatalf("состояние %s, ожидалось %s", after.State, caldron.StatePreparing)
	}

	after, err = env.caldrons.Contribute(ctx, member, pot.Id, 0)
	if err != nil {
		t.Fatalf("взнос участника: %v", err)
	}
	if after.State != caldron.StateReady {
		t.Fatalf("состояние %s, ожидалось %s", after.State, caldron.StateReady)
	}
	if after.Collected != 5_000_00 {
		t.Errorf("собрано %s, ожидалось %s", after.Collected, credit.Amount(5_000_00))
	}
	// Средства лежат на кошельке котла, а не создателя.
	if env.wallet.balanceOf(pot.Id) != 5_000_00 {
		t.Errorf("на кошельке котла %s", env.wallet.balanceOf(pot.Id))
	}

	t.Run("повторный взнос отклоняется", func(t *testing.T) {
		if _, err := env.caldrons.Contribute(ctx, member, pot.Id, 0); !errors.Is(err, ErrAlreadyPaid) {
			t.Errorf("получено %v, ожидалась %v", err, ErrAlreadyPaid)
		}
	})

	t.Run("участники оповещены о готовности", func(t *testing.T) {
		events := env.events.byType(notify.EventCaldronStateChanged)
		if len(events) != 2 {
			t.Fatalf("оповещений о смене состояния %d, ожидалось 2", len(events))
		}
	})
}

func TestContributionChecksWallet(t *testing.T) {
	ctx := context.Background()
	env := newEnvironment(t)
	creator := uuid.New()
	member := uuid.New()
	// У участника не хватает средств: проверка идёт против кошелька,
	// а не на слово.
	env.wallet.fund(creator, 10_000_00)
	env.wallet.fund(member, 100_00)

	pot := env.fixedCaldron(t, creator, 2_500_00, member)
	if _, err := env.caldrons.Contribute(ctx, member, pot.Id, 0); !errors.Is(err, ErrWalletUnavailable) {
		t.Fatalf("получено %v, ожидалась %v", err, ErrWalletUnavailable)
	}

	current, err := env.db.Caldron(ctx, pot.Id)
	if err != nil {
		t.Fatalf("чтение котла: %v", err)
	}
	// Взнос не прошёл — участник не считается внёсшим.
	for _, participant := range current.Participants {
		if participant.UserId == member && participant.State != caldron.ParticipantInvited {
			t.Errorf("участник отмечен внёсшим без перевода: %+v", participant)
		}
	}
}

func TestRangeContribution(t *testing.T) {
	ctx := context.Background()
	env := newEnvironment(t)
	creator := uuid.New()
	member := uuid.New()
	env.wallet.fund(creator, 100_000_00)
	env.wallet.fund(member, 100_000_00)

	pot, err := env.caldrons.Create(ctx, creator, caldron.CreateCaldron{
		Title: "Юбилей", Type: caldron.TypeLuck, Mode: caldron.ModeRange,
		CreatorParticipates: true, MinAmount: 1_000_00, MaxAmount: 5_000_00,
	})
	if err != nil {
		t.Fatalf("создание котла: %v", err)
	}
	if pot, err = env.caldrons.AddParticipant(ctx, creator, pot.Id,
		caldron.AddParticipant{UserId: member}); err != nil {
		t.Fatalf("добавление участника: %v", err)
	}

	if _, err = env.caldrons.Contribute(ctx, member, pot.Id, 9_000_00); !errors.Is(err, caldron.ErrInvalidContribution) {
		t.Errorf("сумма вне диапазона принята: %v", err)
	}
	if _, err = env.caldrons.Contribute(ctx, member, pot.Id, 3_000_00); err != nil {
		t.Fatalf("взнос в пределах диапазона: %v", err)
	}
	if env.wallet.balanceOf(pot.Id) != 3_000_00 {
		t.Errorf("на кошельке котла %s, ожидалось %s",
			env.wallet.balanceOf(pot.Id), credit.Amount(3_000_00))
	}
}

// TestCancelReturnsEverything проверяет требование README: после отмены
// сумма средств в системе совпадает с суммой до сбора.
func TestCancelReturnsEverything(t *testing.T) {
	ctx := context.Background()
	env := newEnvironment(t)
	creator := uuid.New()
	member := uuid.New()
	env.wallet.fund(creator, 10_000_00)
	env.wallet.fund(member, 7_000_00)
	before := env.wallet.total()

	pot := env.fixedCaldron(t, creator, 2_500_00, member)
	if _, err := env.caldrons.Contribute(ctx, creator, pot.Id, 0); err != nil {
		t.Fatalf("взнос создателя: %v", err)
	}
	if _, err := env.caldrons.Contribute(ctx, member, pot.Id, 0); err != nil {
		t.Fatalf("взнос участника: %v", err)
	}

	cancelled, err := env.caldrons.Cancel(ctx, creator, pot.Id)
	if err != nil {
		t.Fatalf("отмена котла: %v", err)
	}
	if cancelled.State != caldron.StateCancelled {
		t.Fatalf("состояние %s, ожидалось %s", cancelled.State, caldron.StateCancelled)
	}

	if env.wallet.balanceOf(creator) != 10_000_00 || env.wallet.balanceOf(member) != 7_000_00 {
		t.Errorf("средства вернулись не полностью: %s и %s",
			env.wallet.balanceOf(creator), env.wallet.balanceOf(member))
	}
	if env.wallet.total() != before {
		t.Errorf("сумма средств в системе изменилась: было %s, стало %s", before, env.wallet.total())
	}
	if env.wallet.balanceOf(pot.Id) != 0 {
		t.Errorf("на кошельке котла осталось %s", env.wallet.balanceOf(pot.Id))
	}
}

// TestRefundIsIdempotent проверяет, что повторный проход возврата
// не возвращает средства дважды.
func TestRefundIsIdempotent(t *testing.T) {
	ctx := context.Background()
	env := newEnvironment(t)
	creator := uuid.New()
	env.wallet.fund(creator, 10_000_00)

	pot := env.fixedCaldron(t, creator, 2_500_00)
	if _, err := env.caldrons.Contribute(ctx, creator, pot.Id, 0); err != nil {
		t.Fatalf("взнос: %v", err)
	}
	if _, err := env.caldrons.Cancel(ctx, creator, pot.Id); err != nil {
		t.Fatalf("отмена: %v", err)
	}

	balance := env.wallet.balanceOf(creator)
	// Фоновая задача проходит по отменённым котлам ещё раз: она не должна
	// вернуть средства повторно.
	cancelled, err := env.db.Caldron(ctx, pot.Id)
	if err != nil {
		t.Fatalf("чтение котла: %v", err)
	}
	for i := range cancelled.Participants {
		cancelled.Participants[i].State = caldron.ParticipantPaid
	}
	if err = env.caldrons.refund(ctx, cancelled); err != nil {
		t.Fatalf("повторный возврат: %v", err)
	}
	if env.wallet.balanceOf(creator) != balance {
		t.Errorf("повторный возврат изменил баланс: было %s, стало %s",
			balance, env.wallet.balanceOf(creator))
	}
}

// TestRefundPendingFinishesInterruptedRefund проверяет добивание возврата,
// прерванного сбоем кошелька.
func TestRefundPendingFinishesInterruptedRefund(t *testing.T) {
	ctx := context.Background()
	env := newEnvironment(t)
	creator := uuid.New()
	member := uuid.New()
	env.wallet.fund(creator, 10_000_00)
	env.wallet.fund(member, 10_000_00)

	pot := env.fixedCaldron(t, creator, 2_500_00, member)
	if _, err := env.caldrons.Contribute(ctx, creator, pot.Id, 0); err != nil {
		t.Fatalf("взнос создателя: %v", err)
	}
	if _, err := env.caldrons.Contribute(ctx, member, pot.Id, 0); err != nil {
		t.Fatalf("взнос участника: %v", err)
	}

	// Кошелёк котла недоступен: отмена проходит, возврат — нет.
	env.wallet.fail[pot.Id] = true
	if _, err := env.caldrons.Cancel(ctx, creator, pot.Id); err != nil {
		t.Fatalf("отмена: %v", err)
	}
	if env.wallet.balanceOf(pot.Id) != 5_000_00 {
		t.Fatalf("средства ушли из котла при недоступном кошельке: %s", env.wallet.balanceOf(pot.Id))
	}

	env.wallet.fail[pot.Id] = false
	if err := env.caldrons.RefundPending(ctx); err != nil {
		t.Fatalf("добивание возвратов: %v", err)
	}
	if env.wallet.balanceOf(creator) != 10_000_00 || env.wallet.balanceOf(member) != 10_000_00 {
		t.Errorf("возврат не добит: %s и %s",
			env.wallet.balanceOf(creator), env.wallet.balanceOf(member))
	}

	current, err := env.db.Caldron(ctx, pot.Id)
	if err != nil {
		t.Fatalf("чтение котла: %v", err)
	}
	for _, participant := range current.Participants {
		if participant.State != caldron.ParticipantRefunded {
			t.Errorf("участник %s остался в состоянии %s", participant.UserId, participant.State)
		}
	}
}

func TestSettleTransfersEverythingToWinner(t *testing.T) {
	ctx := context.Background()
	env := newEnvironment(t)
	creator := uuid.New()
	member := uuid.New()
	env.wallet.fund(creator, 10_000_00)
	env.wallet.fund(member, 10_000_00)
	before := env.wallet.total()

	pot := env.fixedCaldron(t, creator, 2_500_00, member)
	if _, err := env.caldrons.Contribute(ctx, creator, pot.Id, 0); err != nil {
		t.Fatalf("взнос создателя: %v", err)
	}
	if _, err := env.caldrons.Contribute(ctx, member, pot.Id, 0); err != nil {
		t.Fatalf("взнос участника: %v", err)
	}

	settled, err := env.caldrons.Settle(ctx, creator, pot.Id, member)
	if err != nil {
		t.Fatalf("завершение котла: %v", err)
	}
	if settled.State != caldron.StateSettled {
		t.Errorf("состояние %s, ожидалось %s", settled.State, caldron.StateSettled)
	}
	if env.wallet.balanceOf(member) != 12_500_00 {
		t.Errorf("победителю досталось %s, ожидалось %s",
			env.wallet.balanceOf(member), credit.Amount(12_500_00))
	}
	if env.wallet.total() != before {
		t.Errorf("сумма средств в системе изменилась: было %s, стало %s", before, env.wallet.total())
	}
}

func TestSettleRequirements(t *testing.T) {
	ctx := context.Background()
	env := newEnvironment(t)
	creator := uuid.New()
	member := uuid.New()
	env.wallet.fund(creator, 10_000_00)
	env.wallet.fund(member, 10_000_00)
	pot := env.fixedCaldron(t, creator, 2_500_00, member)

	t.Run("несобранный котёл не завершается", func(t *testing.T) {
		if _, err := env.caldrons.Settle(ctx, creator, pot.Id, member); !errors.Is(err, ErrNotReady) {
			t.Errorf("получено %v, ожидалась %v", err, ErrNotReady)
		}
	})

	if _, err := env.caldrons.Contribute(ctx, creator, pot.Id, 0); err != nil {
		t.Fatalf("взнос создателя: %v", err)
	}
	if _, err := env.caldrons.Contribute(ctx, member, pot.Id, 0); err != nil {
		t.Fatalf("взнос участника: %v", err)
	}

	t.Run("победитель обязан быть участником", func(t *testing.T) {
		if _, err := env.caldrons.Settle(ctx, creator, pot.Id, uuid.New()); !errors.Is(err, ErrForbidden) {
			t.Errorf("получено %v, ожидалась %v", err, ErrForbidden)
		}
	})

	t.Run("завершает только создатель", func(t *testing.T) {
		if _, err := env.caldrons.Settle(ctx, member, pot.Id, member); !errors.Is(err, ErrForbidden) {
			t.Errorf("получено %v, ожидалась %v", err, ErrForbidden)
		}
	})
}

func TestParticipantsCannotBeAddedAfterReady(t *testing.T) {
	ctx := context.Background()
	env := newEnvironment(t)
	creator := uuid.New()
	env.wallet.fund(creator, 10_000_00)

	pot := env.fixedCaldron(t, creator, 2_500_00)
	if _, err := env.caldrons.Contribute(ctx, creator, pot.Id, 0); err != nil {
		t.Fatalf("взнос: %v", err)
	}

	// Готовый котёл уже собран: новый участник сделал бы собранную сумму
	// неверной.
	if _, err := env.caldrons.AddParticipant(ctx, creator, pot.Id,
		caldron.AddParticipant{UserId: uuid.New()}); !errors.Is(err, caldron.ErrInvalidTransition) {
		t.Errorf("получено %v, ожидалась %v", err, caldron.ErrInvalidTransition)
	}
}
