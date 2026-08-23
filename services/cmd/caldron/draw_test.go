package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"

	"wish/services/shared/caldron"
	"wish/services/shared/credit"
	"wish/services/shared/marketplace"
	"wish/services/shared/notify"

	"github.com/google/uuid"
)

// cheapGift подбирает товар не дороже предела. Заглушка выводит цену
// из идентификатора, поэтому дешёвый товар ищется перебором — зато цена
// в тесте настоящая, а не подставленная мимо площадки.
func cheapGift(t *testing.T, limit credit.Amount, salt string) GiftRequest {
	t.Helper()

	stub := &marketplace.Stub{}
	for i := range 500 {
		id := fmt.Sprintf("%s-%d", salt, i)
		product, err := stub.Product(context.Background(), id)
		if err != nil {
			t.Fatalf("карточка товара: %v", err)
		}
		if product.Price <= limit {
			return GiftRequest{Provider: marketplace.ProviderStub, ProductId: id}
		}
	}
	t.Fatalf("не нашлось товара дешевле %s", limit)
	return GiftRequest{}
}

// giftCaldron собирает котёл подарков со списками участников и взносами.
func (e *environment) giftCaldron(
	t *testing.T,
	creator uuid.UUID,
	members ...uuid.UUID,
) caldron.Caldron {
	t.Helper()
	ctx := context.Background()

	pot := e.fixedCaldron(t, creator, 2_500_00, members...)
	for _, user := range append([]uuid.UUID{creator}, members...) {
		if _, err := e.caldrons.SetGifts(ctx, user, pot.Id, []GiftRequest{
			cheapGift(t, 2_000_00, "gift-"+user.String()[:8]),
		}); err != nil {
			t.Fatalf("список подарков: %v", err)
		}
		if _, err := e.caldrons.Contribute(ctx, user, pot.Id, 0); err != nil {
			t.Fatalf("взнос: %v", err)
		}
	}

	current, err := e.db.Caldron(ctx, pot.Id)
	if err != nil {
		t.Fatalf("чтение котла: %v", err)
	}
	return current
}

func TestDrawIsVerifiable(t *testing.T) {
	ctx := context.Background()
	env := newEnvironment(t)
	creator := uuid.New()
	member := uuid.New()
	env.wallet.fund(creator, 100_000_00)
	env.wallet.fund(member, 100_000_00)

	pot := env.giftCaldron(t, creator, member)
	if pot.State != caldron.StateReady {
		t.Fatalf("состояние %s, ожидалось %s", pot.State, caldron.StateReady)
	}
	if pot.Commitment == "" {
		t.Fatal("обязательство не опубликовано до розыгрыша")
	}

	draw, err := env.caldrons.Draw(ctx, creator, pot.Id)
	if err != nil {
		t.Fatalf("розыгрыш: %v", err)
	}

	// Главное свойство: участник может пересчитать результат сам.
	seed, err := hex.DecodeString(draw.Seed)
	if err != nil {
		t.Fatalf("разбор зерна: %v", err)
	}
	if !caldron.VerifyCommitment(seed, pot.Commitment) {
		t.Error("раскрытое зерно не соответствует опубликованному обязательству")
	}
	recomputed, err := caldron.SelectWinner(seed, []uuid.UUID{creator, member})
	if err != nil {
		t.Fatalf("пересчёт победителя: %v", err)
	}
	if recomputed != draw.WinnerId {
		t.Errorf("пересчёт дал другого победителя: %s вместо %s", recomputed, draw.WinnerId)
	}

	if draw.WinnerId != creator && draw.WinnerId != member {
		t.Errorf("победитель не из числа участников: %s", draw.WinnerId)
	}
	if draw.Total+draw.Payout != pot.Collected {
		t.Errorf("подарки %s и остаток %s не дают суммы котла %s",
			draw.Total, draw.Payout, pot.Collected)
	}
	if draw.Total > pot.Collected {
		t.Errorf("набор дороже котла: %s при %s", draw.Total, pot.Collected)
	}
}

func TestDrawSettlesCaldron(t *testing.T) {
	ctx := context.Background()
	env := newEnvironment(t)
	creator := uuid.New()
	member := uuid.New()
	env.wallet.fund(creator, 10_000_00)
	env.wallet.fund(member, 10_000_00)
	before := env.wallet.total()

	pot := env.giftCaldron(t, creator, member)
	draw, err := env.caldrons.Draw(ctx, creator, pot.Id)
	if err != nil {
		t.Fatalf("розыгрыш: %v", err)
	}

	current, err := env.db.Caldron(ctx, pot.Id)
	if err != nil {
		t.Fatalf("чтение котла: %v", err)
	}
	if current.State != caldron.StateSettled {
		t.Errorf("состояние %s, ожидалось %s", current.State, caldron.StateSettled)
	}
	// Победителю уходит вся сумма котла: заказать подарки за него площадка
	// не позволяет, и выпавший набор — это то, на что он потратит деньги.
	// Свой взнос он тоже вносил, поэтому баланс — начальный минус взнос
	// плюс весь котёл.
	if env.wallet.balanceOf(draw.WinnerId) != 10_000_00-2_500_00+pot.Collected {
		t.Errorf("победителю досталось %s", env.wallet.balanceOf(draw.WinnerId))
	}
	if env.wallet.total() != before {
		t.Errorf("сумма средств в системе изменилась: было %s, стало %s", before, env.wallet.total())
	}

	t.Run("участники оповещены об итогах", func(t *testing.T) {
		events := env.events.byType(notify.EventCaldronDrawResult)
		if len(events) != 2 {
			t.Fatalf("оповещений об итогах %d, ожидалось 2", len(events))
		}
		for _, event := range events {
			if event.Payload["winner"] == "" {
				t.Error("в оповещении не сказано, кто выиграл")
			}
		}
	})
}

// TestDrawIsIdempotent проверяет, что обрыв связи после розыгрыша
// не приводит ко второму исходу.
func TestDrawIsIdempotent(t *testing.T) {
	ctx := context.Background()
	env := newEnvironment(t)
	creator := uuid.New()
	member := uuid.New()
	env.wallet.fund(creator, 10_000_00)
	env.wallet.fund(member, 10_000_00)
	pot := env.giftCaldron(t, creator, member)

	first, err := env.caldrons.Draw(ctx, creator, pot.Id)
	if err != nil {
		t.Fatalf("розыгрыш: %v", err)
	}
	balance := env.wallet.balanceOf(first.WinnerId)

	for range 3 {
		again, err := env.caldrons.Draw(ctx, creator, pot.Id)
		if err != nil {
			t.Fatalf("повторный розыгрыш: %v", err)
		}
		if again.WinnerId != first.WinnerId || again.Seed != first.Seed {
			t.Fatalf("повтор переиграл исход: %s и %s", first.WinnerId, again.WinnerId)
		}
	}
	if env.wallet.balanceOf(first.WinnerId) != balance {
		t.Errorf("повтор перевёл средства второй раз: %s вместо %s",
			env.wallet.balanceOf(first.WinnerId), balance)
	}
}

func TestDrawPermissions(t *testing.T) {
	ctx := context.Background()
	env := newEnvironment(t)
	creator := uuid.New()
	member := uuid.New()
	env.wallet.fund(creator, 10_000_00)
	env.wallet.fund(member, 10_000_00)
	pot := env.giftCaldron(t, creator, member)

	t.Run("участник не запускает розыгрыш", func(t *testing.T) {
		if _, err := env.caldrons.Draw(ctx, member, pot.Id); !errors.Is(err, ErrForbidden) {
			t.Errorf("получено %v, ожидалась %v", err, ErrForbidden)
		}
	})

	t.Run("посторонний не видит котла", func(t *testing.T) {
		if _, err := env.caldrons.Draw(ctx, uuid.New(), pot.Id); !errors.Is(err, ErrNotFound) {
			t.Errorf("получено %v, ожидалась %v", err, ErrNotFound)
		}
	})

	t.Run("назначенный арбитр запускает розыгрыш", func(t *testing.T) {
		if _, err := env.caldrons.SetArbiter(ctx, creator, pot.Id, member); err != nil {
			t.Fatalf("назначение арбитра: %v", err)
		}
		if _, err := env.caldrons.Draw(ctx, member, pot.Id); err != nil {
			t.Errorf("арбитр не смог запустить розыгрыш: %v", err)
		}
	})

	t.Run("арбитр выбирается из участников", func(t *testing.T) {
		if _, err := env.caldrons.SetArbiter(ctx, creator, pot.Id, uuid.New()); !errors.Is(err, ErrForbidden) {
			t.Errorf("посторонний назначен арбитром: %v", err)
		}
	})
}

func TestDrawRequiresReadyCaldron(t *testing.T) {
	ctx := context.Background()
	env := newEnvironment(t)
	creator := uuid.New()
	member := uuid.New()
	env.wallet.fund(creator, 10_000_00)
	pot := env.fixedCaldron(t, creator, 2_500_00, member)

	// Внесли не все: разыгрывать нечего.
	if _, err := env.caldrons.Contribute(ctx, creator, pot.Id, 0); err != nil {
		t.Fatalf("взнос: %v", err)
	}
	if _, err := env.caldrons.Draw(ctx, creator, pot.Id); !errors.Is(err, ErrNotReady) {
		t.Errorf("получено %v, ожидалась %v", err, ErrNotReady)
	}
}

func TestGiftListLimits(t *testing.T) {
	ctx := context.Background()
	env := newEnvironment(t)
	creator := uuid.New()
	env.wallet.fund(creator, 100_000_00)
	pot := env.fixedCaldron(t, creator, 2_500_00)

	t.Run("больше пяти подарков не принимается", func(t *testing.T) {
		requests := make([]GiftRequest, caldron.MaxGifts+1)
		for i := range requests {
			requests[i] = GiftRequest{Provider: marketplace.ProviderStub,
				ProductId: "gift-" + string(rune('a'+i))}
		}
		if _, err := env.caldrons.SetGifts(ctx, creator, pot.Id, requests); !errors.Is(err, caldron.ErrTooManyGifts) {
			t.Errorf("получено %v, ожидалась %v", err, caldron.ErrTooManyGifts)
		}
	})

	t.Run("список дороже котла отклоняется", func(t *testing.T) {
		// Заглушка выдаёт цены до 100 000 рублей, а котёл рассчитан
		// на 2 500: рано или поздно список окажется слишком дорогим.
		expensive := false
		for i := range 20 {
			_, err := env.caldrons.SetGifts(ctx, creator, pot.Id, []GiftRequest{
				{Provider: marketplace.ProviderStub, ProductId: "expensive-" + string(rune('a'+i))},
			})
			if errors.Is(err, caldron.ErrGiftsTooExpensive) {
				expensive = true
				break
			}
		}
		if !expensive {
			t.Error("ни один дорогой список не был отклонён")
		}
	})

	t.Run("чужой список не виден", func(t *testing.T) {
		stranger := uuid.New()
		if _, err := env.caldrons.Gifts(ctx, stranger, pot.Id); !errors.Is(err, ErrNotFound) {
			t.Errorf("получено %v, ожидалась %v", err, ErrNotFound)
		}
	})
}

func TestGiftsTakePriceFromMarketplace(t *testing.T) {
	ctx := context.Background()
	env := newEnvironment(t)
	creator := uuid.New()
	env.wallet.fund(creator, 100_000_00)
	pot := env.fixedCaldron(t, creator, 100_000_00)

	gifts, err := env.caldrons.SetGifts(ctx, creator, pot.Id, []GiftRequest{
		cheapGift(t, 100_000_00, "coffee"),
	})
	if err != nil {
		t.Fatalf("список подарков: %v", err)
	}
	if len(gifts) != 1 {
		t.Fatalf("подарков %d, ожидался 1", len(gifts))
	}
	if gifts[0].Price <= 0 || gifts[0].Title == "" {
		t.Errorf("карточка не перенесена в подарок: %+v", gifts[0])
	}
	if gifts[0].PriceAt.IsZero() {
		t.Error("нет отметки времени цены: на площадке цена меняется")
	}
}

func TestDrawResultVisibleToParticipants(t *testing.T) {
	ctx := context.Background()
	env := newEnvironment(t)
	creator := uuid.New()
	member := uuid.New()
	env.wallet.fund(creator, 10_000_00)
	env.wallet.fund(member, 10_000_00)
	pot := env.giftCaldron(t, creator, member)

	if _, err := env.caldrons.DrawResult(ctx, member, pot.Id); !errors.Is(err, ErrNoDraw) {
		t.Errorf("до розыгрыша получено %v, ожидалась %v", err, ErrNoDraw)
	}

	drawn, err := env.caldrons.Draw(ctx, creator, pot.Id)
	if err != nil {
		t.Fatalf("розыгрыш: %v", err)
	}

	result, err := env.caldrons.DrawResult(ctx, member, pot.Id)
	if err != nil {
		t.Fatalf("чтение результата участником: %v", err)
	}
	if result.WinnerId != drawn.WinnerId || result.Seed != drawn.Seed {
		t.Error("участник видит не тот результат")
	}
	if _, err = env.caldrons.DrawResult(ctx, uuid.New(), pot.Id); !errors.Is(err, ErrNotFound) {
		t.Errorf("посторонний увидел результат: %v", err)
	}
}
