package main

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"wish/services/shared/caldron"
	"wish/services/shared/credit"
	"wish/services/shared/marketplace"
	"wish/services/shared/notify"

	"github.com/google/uuid"
)

// countingCatalog считает обращения к площадке: котлу удачи они не нужны
// вовсе, и это стоит проверять, а не предполагать.
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
	c.calls++
	return c.stub.Order(ctx, id, address)
}

// luckCaldron собирает котёл удачи с внесёнными взносами.
func (e *environment) luckCaldron(
	t *testing.T,
	creator uuid.UUID,
	amount credit.Amount,
	members ...uuid.UUID,
) caldron.Caldron {
	t.Helper()
	ctx := context.Background()

	pot, err := e.caldrons.Create(ctx, creator, caldron.CreateCaldron{
		Title: "Котёл удачи", Type: caldron.TypeLuck, Mode: caldron.ModeFixed,
		CreatorParticipates: true, Amount: amount,
	})
	if err != nil {
		t.Fatalf("создание котла: %v", err)
	}
	for _, member := range members {
		if _, err = e.caldrons.AddParticipant(ctx, creator, pot.Id,
			caldron.AddParticipant{UserId: member}); err != nil {
			t.Fatalf("добавление участника: %v", err)
		}
	}
	for _, user := range append([]uuid.UUID{creator}, members...) {
		if _, err = e.caldrons.Contribute(ctx, user, pot.Id, 0); err != nil {
			t.Fatalf("взнос: %v", err)
		}
	}

	current, err := e.db.Caldron(ctx, pot.Id)
	if err != nil {
		t.Fatalf("чтение котла: %v", err)
	}
	return current
}

// TestLuckDrawGivesEverythingToOne проверяет требование README: вся сумма
// котла удачи достаётся одному участнику, а в системе денег не прибавилось.
func TestLuckDrawGivesEverythingToOne(t *testing.T) {
	ctx := context.Background()
	env := newEnvironment(t)
	creator := uuid.New()
	first := uuid.New()
	second := uuid.New()
	for _, user := range []uuid.UUID{creator, first, second} {
		env.wallet.fund(user, 10_000_00)
	}
	before := env.wallet.total()

	pot := env.luckCaldron(t, creator, 2_500_00, first, second)
	if pot.State != caldron.StateReady {
		t.Fatalf("состояние %s, ожидалось %s", pot.State, caldron.StateReady)
	}
	if pot.Collected != 7_500_00 {
		t.Fatalf("собрано %s, ожидалось %s", pot.Collected, credit.Amount(7_500_00))
	}

	draw, err := env.caldrons.Draw(ctx, creator, pot.Id)
	if err != nil {
		t.Fatalf("розыгрыш: %v", err)
	}

	// В котле удачи подарков нет: победителю уходит вся сумма.
	if len(draw.Gifts) != 0 || draw.Total != 0 {
		t.Errorf("в котле удачи выпали подарки: %+v", draw.Gifts)
	}
	if draw.Payout != pot.Collected {
		t.Errorf("к выплате %s, ожидалось %s", draw.Payout, pot.Collected)
	}

	winners := 0
	for _, user := range []uuid.UUID{creator, first, second} {
		balance := env.wallet.balanceOf(user)
		switch {
		case user == draw.WinnerId:
			winners++
			if balance != 10_000_00-2_500_00+pot.Collected {
				t.Errorf("победителю досталось %s", balance)
			}
		case balance != 10_000_00-2_500_00:
			t.Errorf("у участника %s баланс %s", user, balance)
		}
	}
	if winners != 1 {
		t.Errorf("победителей %d, ожидался один", winners)
	}
	if env.wallet.total() != before {
		t.Errorf("сумма средств в системе изменилась: было %s, стало %s", before, env.wallet.total())
	}

	current, err := env.db.Caldron(ctx, pot.Id)
	if err != nil {
		t.Fatalf("чтение котла: %v", err)
	}
	if current.State != caldron.StateSettled {
		t.Errorf("состояние %s, ожидалось %s", current.State, caldron.StateSettled)
	}
}

// TestLuckDrawIsVerifiable проверяет, что котёл удачи разыгрывается тем же
// проверяемым механизмом, что и котёл подарков, а не второй реализацией.
func TestLuckDrawIsVerifiable(t *testing.T) {
	ctx := context.Background()
	env := newEnvironment(t)
	creator := uuid.New()
	member := uuid.New()
	env.wallet.fund(creator, 10_000_00)
	env.wallet.fund(member, 10_000_00)

	pot := env.luckCaldron(t, creator, 2_500_00, member)
	draw, err := env.caldrons.Draw(ctx, creator, pot.Id)
	if err != nil {
		t.Fatalf("розыгрыш: %v", err)
	}

	seed, err := hex.DecodeString(draw.Seed)
	if err != nil {
		t.Fatalf("разбор зерна: %v", err)
	}
	if !caldron.VerifyCommitment(seed, pot.Commitment) {
		t.Error("раскрытое зерно не соответствует обязательству")
	}
	recomputed, err := caldron.SelectWinner(seed, []uuid.UUID{creator, member})
	if err != nil {
		t.Fatalf("пересчёт победителя: %v", err)
	}
	if recomputed != draw.WinnerId {
		t.Errorf("пересчёт дал другого победителя: %s вместо %s", recomputed, draw.WinnerId)
	}

	t.Run("участники оповещены об итогах", func(t *testing.T) {
		events := env.events.byType(notify.EventCaldronDrawResult)
		if len(events) != 2 {
			t.Fatalf("оповещений %d, ожидалось 2", len(events))
		}
		for _, event := range events {
			if event.UserId == draw.WinnerId && event.Payload["winner"] != "вы" {
				t.Errorf("победителю сообщили %q", event.Payload["winner"])
			}
		}
	})
}

// TestLuckDrawDoesNotTouchMarketplace фиксирует, что котёл удачи проще:
// подарков в нём нет, и обращаться к площадке незачем.
func TestLuckDrawDoesNotTouchMarketplace(t *testing.T) {
	ctx := context.Background()
	env := newEnvironment(t)
	catalog := &countingCatalog{}
	env.caldrons.catalogs = marketplace.NewRegistry(catalog)

	creator := uuid.New()
	member := uuid.New()
	env.wallet.fund(creator, 10_000_00)
	env.wallet.fund(member, 10_000_00)

	pot := env.luckCaldron(t, creator, 2_500_00, member)
	if _, err := env.caldrons.Draw(ctx, creator, pot.Id); err != nil {
		t.Fatalf("розыгрыш: %v", err)
	}
	if catalog.calls != 0 {
		t.Errorf("обращений к площадке %d, ожидалось ноль", catalog.calls)
	}
}

func TestLuckCaldronRejectsGifts(t *testing.T) {
	ctx := context.Background()
	env := newEnvironment(t)
	creator := uuid.New()
	env.wallet.fund(creator, 10_000_00)

	pot, err := env.caldrons.Create(ctx, creator, caldron.CreateCaldron{
		Title: "Котёл удачи", Type: caldron.TypeLuck, Mode: caldron.ModeFixed,
		CreatorParticipates: true, Amount: 2_500_00,
	})
	if err != nil {
		t.Fatalf("создание котла: %v", err)
	}

	// Список подарков — принадлежность котла подарков; в котле удачи
	// он ни на что не влияет, и принимать его значило бы обманывать
	// участника.
	if _, err = env.caldrons.SetGifts(ctx, creator, pot.Id, []GiftRequest{
		{Provider: marketplace.ProviderStub, ProductId: "coffee-machine"},
	}); !errors.Is(err, ErrForbidden) {
		t.Errorf("получено %v, ожидалась %v", err, ErrForbidden)
	}
}

func TestLuckDrawByArbiter(t *testing.T) {
	ctx := context.Background()
	env := newEnvironment(t)
	creator := uuid.New()
	member := uuid.New()
	env.wallet.fund(creator, 10_000_00)
	env.wallet.fund(member, 10_000_00)

	pot, err := env.caldrons.Create(ctx, creator, caldron.CreateCaldron{
		Title: "Котёл удачи", Type: caldron.TypeLuck, Mode: caldron.ModeFixed,
		// Создатель — арбитр: он организует сбор и не участвует в нём,
		// поэтому выиграть не может.
		CreatorParticipates: false, Amount: 2_500_00,
	})
	if err != nil {
		t.Fatalf("создание котла: %v", err)
	}
	if _, err = env.caldrons.AddParticipant(ctx, creator, pot.Id,
		caldron.AddParticipant{UserId: member}); err != nil {
		t.Fatalf("добавление участника: %v", err)
	}
	if _, err = env.caldrons.Contribute(ctx, member, pot.Id, 0); err != nil {
		t.Fatalf("взнос: %v", err)
	}

	draw, err := env.caldrons.Draw(ctx, creator, pot.Id)
	if err != nil {
		t.Fatalf("розыгрыш: %v", err)
	}
	if draw.WinnerId != member {
		t.Errorf("победитель %s, ожидался единственный участник %s", draw.WinnerId, member)
	}
	if env.wallet.balanceOf(creator) != 10_000_00 {
		t.Errorf("арбитр участвовал в сборе: баланс %s", env.wallet.balanceOf(creator))
	}
}
