//go:build integration

package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"wish/services/shared/caldron"
	"wish/services/shared/credit"
	"wish/services/testsupport"

	"github.com/google/uuid"
)

func newTestDatabase(t *testing.T) Database {
	t.Helper()
	db, err := NewDatabase(context.Background(), testsupport.Prepare(t, "caldron"))
	if err != nil {
		t.Fatalf("не удалось открыть репозиторий: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func createFixed(t *testing.T, db Database, creator uuid.UUID, amount credit.Amount) caldron.Caldron {
	t.Helper()
	pot, err := db.Create(context.Background(), caldron.Caldron{
		CreatorId: creator, Title: "Юбилей", Type: caldron.TypeGift,
		CreatorParticipates: true, Mode: caldron.ModeFixed, Amount: amount,
	})
	if err != nil {
		t.Fatalf("создание котла: %v", err)
	}
	return pot
}

func TestSchemaRejectsInconsistentCaldron(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	creator := uuid.New()

	// Проверка в схеме, а не только в коде: запись мимо сервиса не должна
	// оставить котёл без правила, по которому считается взнос.
	if _, err := db.Create(ctx, caldron.Caldron{
		CreatorId: creator, Title: "Без суммы", Type: caldron.TypeGift, Mode: caldron.ModeFixed,
	}); err == nil {
		t.Error("котёл с точной суммой записан без суммы")
	}

	if _, err := db.Create(ctx, caldron.Caldron{
		CreatorId: creator, Title: "Перевёрнутый диапазон", Type: caldron.TypeLuck,
		Mode: caldron.ModeRange, MinAmount: 5_000_00, MaxAmount: 1_000_00,
	}); err == nil {
		t.Error("котёл с перевёрнутым диапазоном записан")
	}

	if _, err := db.Create(ctx, caldron.Caldron{
		CreatorId: creator, Title: "Лишняя сумма", Type: caldron.TypeGift,
		Mode: caldron.ModeIndividual, Amount: 1_000_00,
	}); err == nil {
		t.Error("индивидуальный котёл записан с общей суммой")
	}
}

func TestParticipantsLifecycle(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	creator := uuid.New()
	member := uuid.New()
	pot := createFixed(t, db, creator, 2_500_00)

	updated, err := db.AddParticipant(ctx, pot.Id, caldron.AddParticipant{UserId: member})
	if err != nil {
		t.Fatalf("добавление участника: %v", err)
	}
	if len(updated.Participants) != 1 {
		t.Fatalf("участников %d, ожидался 1", len(updated.Participants))
	}
	// В режиме точной суммы ожидаемый взнос берётся из котла.
	if updated.Participants[0].Expected != 2_500_00 {
		t.Errorf("ожидаемый взнос %s", updated.Participants[0].Expected)
	}

	t.Run("повторное добавление ничего не ломает", func(t *testing.T) {
		again, err := db.AddParticipant(ctx, pot.Id, caldron.AddParticipant{UserId: member})
		if err != nil {
			t.Fatalf("повторное добавление: %v", err)
		}
		if len(again.Participants) != 1 {
			t.Errorf("участников %d, ожидался 1", len(again.Participants))
		}
	})

	t.Run("участник удаляется, пока не внёс", func(t *testing.T) {
		after, err := db.RemoveParticipant(ctx, pot.Id, member)
		if err != nil {
			t.Fatalf("удаление участника: %v", err)
		}
		if len(after.Participants) != 0 {
			t.Errorf("участников %d, ожидалось 0", len(after.Participants))
		}
	})

	t.Run("несуществующий участник не удаляется", func(t *testing.T) {
		if _, err := db.RemoveParticipant(ctx, pot.Id, uuid.New()); !errors.Is(err, ErrParticipantNotFound) {
			t.Errorf("получено %v, ожидалась %v", err, ErrParticipantNotFound)
		}
	})
}

func TestReadyWhenEveryoneContributed(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	creator := uuid.New()
	member := uuid.New()
	pot := createFixed(t, db, creator, 2_500_00)

	for _, user := range []uuid.UUID{creator, member} {
		if _, err := db.AddParticipant(ctx, pot.Id, caldron.AddParticipant{UserId: user}); err != nil {
			t.Fatalf("добавление участника: %v", err)
		}
	}

	after, err := db.MarkPaid(ctx, pot.Id, creator, 2_500_00)
	if err != nil {
		t.Fatalf("отметка взноса: %v", err)
	}
	if after.State != caldron.StatePreparing {
		t.Fatalf("состояние %s, ожидалось %s", after.State, caldron.StatePreparing)
	}

	after, err = db.MarkPaid(ctx, pot.Id, member, 2_500_00)
	if err != nil {
		t.Fatalf("отметка взноса: %v", err)
	}
	// Котёл готов по факту последнего взноса, а не по чьей-то команде.
	if after.State != caldron.StateReady {
		t.Fatalf("состояние %s, ожидалось %s", after.State, caldron.StateReady)
	}
	if after.Collected != 5_000_00 {
		t.Errorf("собрано %s, ожидалось %s", after.Collected, credit.Amount(5_000_00))
	}

	t.Run("участника нельзя добавить в готовый котёл", func(t *testing.T) {
		_, err := db.AddParticipant(ctx, pot.Id, caldron.AddParticipant{UserId: uuid.New()})
		if !errors.Is(err, caldron.ErrInvalidTransition) {
			t.Errorf("получено %v, ожидалась %v", err, caldron.ErrInvalidTransition)
		}
	})

	t.Run("взнос в готовый котёл не принимается", func(t *testing.T) {
		_, _, err := db.StartContribution(ctx, pot.Id, member, 0)
		if !errors.Is(err, ErrAlreadyPaid) {
			t.Errorf("получено %v, ожидалась %v", err, ErrAlreadyPaid)
		}
	})
}

// TestConcurrentContribution проверяет, что параллельные запросы одного
// участника не удваивают взнос: отметка ставится только из состояния
// «приглашён».
func TestConcurrentContribution(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	creator := uuid.New()
	pot := createFixed(t, db, creator, 2_500_00)
	if _, err := db.AddParticipant(ctx, pot.Id, caldron.AddParticipant{UserId: creator}); err != nil {
		t.Fatalf("добавление участника: %v", err)
	}

	const attempts = 8
	var wg sync.WaitGroup
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := db.MarkPaid(ctx, pot.Id, creator, 2_500_00); err != nil {
				t.Errorf("отметка взноса: %v", err)
			}
		}()
	}
	wg.Wait()

	current, err := db.Caldron(ctx, pot.Id)
	if err != nil {
		t.Fatalf("чтение котла: %v", err)
	}
	if current.Collected != 2_500_00 {
		t.Errorf("собрано %s, ожидалось %s", current.Collected, credit.Amount(2_500_00))
	}
	if current.State != caldron.StateReady {
		t.Errorf("состояние %s, ожидалось %s", current.State, caldron.StateReady)
	}
}

func TestTransitions(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	creator := uuid.New()
	pot := createFixed(t, db, creator, 2_500_00)

	t.Run("создатель не объявляет котёл готовым", func(t *testing.T) {
		_, err := db.Transition(ctx, pot.Id, caldron.StateReady, caldron.ActorCreator)
		if !errors.Is(err, caldron.ErrForbiddenTransition) {
			t.Errorf("получено %v, ожидалась %v", err, caldron.ErrForbiddenTransition)
		}
	})

	cancelled, err := db.Transition(ctx, pot.Id, caldron.StateCancelled, caldron.ActorCreator)
	if err != nil {
		t.Fatalf("отмена: %v", err)
	}
	if cancelled.CancelledAt == nil {
		t.Error("время отмены не проставлено")
	}

	t.Run("отменённый котёл не оживает", func(t *testing.T) {
		_, err := db.Transition(ctx, pot.Id, caldron.StatePreparing, caldron.ActorSystem)
		if !errors.Is(err, caldron.ErrInvalidTransition) {
			t.Errorf("получено %v, ожидалась %v", err, caldron.ErrInvalidTransition)
		}
	})
}

func TestPendingRefunds(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	creator := uuid.New()
	pot := createFixed(t, db, creator, 2_500_00)
	if _, err := db.AddParticipant(ctx, pot.Id, caldron.AddParticipant{UserId: creator}); err != nil {
		t.Fatalf("добавление участника: %v", err)
	}
	if _, err := db.MarkPaid(ctx, pot.Id, creator, 2_500_00); err != nil {
		t.Fatalf("отметка взноса: %v", err)
	}

	t.Run("собранный котёл в очередь возвратов не попадает", func(t *testing.T) {
		pending, err := db.PendingRefunds(ctx, 10)
		if err != nil {
			t.Fatalf("чтение очереди возвратов: %v", err)
		}
		if len(pending) != 0 {
			t.Errorf("в очереди %d котлов, ожидалось 0", len(pending))
		}
	})

	if _, err := db.Transition(ctx, pot.Id, caldron.StateCancelled, caldron.ActorCreator); err != nil {
		t.Fatalf("отмена: %v", err)
	}

	pending, err := db.PendingRefunds(ctx, 10)
	if err != nil {
		t.Fatalf("чтение очереди возвратов: %v", err)
	}
	if len(pending) != 1 || pending[0].Id != pot.Id {
		t.Fatalf("в очереди %d котлов, ожидался один отменённый", len(pending))
	}
	if len(pending[0].Participants) != 1 {
		t.Errorf("участники не подгружены: %+v", pending[0])
	}

	if err = db.MarkRefunded(ctx, pot.Id, creator); err != nil {
		t.Fatalf("отметка возврата: %v", err)
	}
	pending, err = db.PendingRefunds(ctx, 10)
	if err != nil {
		t.Fatalf("чтение очереди возвратов: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("возвращённый котёл остался в очереди: %+v", pending)
	}
}

func TestByUser(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	creator := uuid.New()
	member := uuid.New()
	stranger := uuid.New()

	pot := createFixed(t, db, creator, 2_500_00)
	if _, err := db.AddParticipant(ctx, pot.Id, caldron.AddParticipant{UserId: member}); err != nil {
		t.Fatalf("добавление участника: %v", err)
	}

	for name, user := range map[string]uuid.UUID{"создатель": creator, "участник": member} {
		caldrons, err := db.ByUser(ctx, user)
		if err != nil {
			t.Fatalf("чтение котлов (%s): %v", name, err)
		}
		if len(caldrons) != 1 {
			t.Errorf("%s видит %d котлов, ожидался 1", name, len(caldrons))
		}
	}

	caldrons, err := db.ByUser(ctx, stranger)
	if err != nil {
		t.Fatalf("чтение котлов постороннего: %v", err)
	}
	if len(caldrons) != 0 {
		t.Errorf("посторонний видит %d котлов", len(caldrons))
	}
}

func TestSetWalletOnce(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	pot := createFixed(t, db, uuid.New(), 2_500_00)

	wallet := uuid.New()
	if err := db.SetWallet(ctx, pot.Id, wallet); err != nil {
		t.Fatalf("присвоение кошелька: %v", err)
	}
	// Смена кошелька у котла со средствами означала бы потерю собранного.
	if err := db.SetWallet(ctx, pot.Id, uuid.New()); err != nil {
		t.Fatalf("повторное присвоение: %v", err)
	}

	current, err := db.Caldron(ctx, pot.Id)
	if err != nil {
		t.Fatalf("чтение котла: %v", err)
	}
	if current.WalletId == nil || *current.WalletId != wallet {
		t.Errorf("кошелёк котла подменён: %+v", current.WalletId)
	}
}

func TestGiftsAndSeed(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	creator := uuid.New()
	member := uuid.New()
	pot := createFixed(t, db, creator, 2_500_00)

	seed, commitment, err := db.Seed(ctx, pot.Id)
	if err != nil {
		t.Fatalf("чтение зерна: %v", err)
	}
	if len(seed) != caldron.SeedSize {
		t.Fatalf("длина зерна %d, ожидалось %d", len(seed), caldron.SeedSize)
	}
	// Обязательство заводится вместе с котлом: подобрать исход задним
	// числом нельзя, если хеш зерна опубликован до розыгрыша.
	if commitment != caldron.Commit(seed) {
		t.Error("обязательство не соответствует зерну")
	}
	if pot.Commitment != commitment {
		t.Errorf("обязательство котла %q не совпало с сохранённым", pot.Commitment)
	}

	gift := caldron.Gift{
		Provider: "STUB", ProductId: "coffee-machine", Title: "Кофеварка",
		URL:   "https://example.invalid/product/coffee-machine",
		Price: 1_000_00, PriceAt: time.Now(),
	}
	saved, err := db.ReplaceGifts(ctx, pot.Id, creator, []caldron.Gift{gift})
	if err != nil {
		t.Fatalf("сохранение подарков: %v", err)
	}
	if len(saved) != 1 || saved[0].Price != 1_000_00 {
		t.Fatalf("подарки сохранены неверно: %+v", saved)
	}

	t.Run("список заменяется целиком", func(t *testing.T) {
		second := gift
		second.ProductId = "kettle"
		second.Title = "Чайник"
		replaced, err := db.ReplaceGifts(ctx, pot.Id, creator, []caldron.Gift{second})
		if err != nil {
			t.Fatalf("замена списка: %v", err)
		}
		if len(replaced) != 1 || replaced[0].ProductId != "kettle" {
			t.Errorf("список не заменён: %+v", replaced)
		}
	})

	t.Run("чужие подарки не затрагиваются", func(t *testing.T) {
		other := gift
		other.ProductId = "blender"
		if _, err := db.ReplaceGifts(ctx, pot.Id, member, []caldron.Gift{other}); err != nil {
			t.Fatalf("сохранение чужих подарков: %v", err)
		}
		if _, err := db.ReplaceGifts(ctx, pot.Id, creator, nil); err != nil {
			t.Fatalf("очистка своего списка: %v", err)
		}

		all, err := db.Gifts(ctx, pot.Id, nil)
		if err != nil {
			t.Fatalf("чтение подарков котла: %v", err)
		}
		if len(all) != 1 || all[0].UserId != member {
			t.Errorf("чужой список пострадал: %+v", all)
		}
	})
}

func TestDrawIsStoredOnce(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	creator := uuid.New()
	pot := createFixed(t, db, creator, 2_500_00)
	seed, commitment, err := db.Seed(ctx, pot.Id)
	if err != nil {
		t.Fatalf("чтение зерна: %v", err)
	}

	if _, err = db.Draw(ctx, pot.Id); !errors.Is(err, ErrNoDraw) {
		t.Errorf("до розыгрыша получено %v, ожидалась %v", err, ErrNoDraw)
	}

	draw := caldron.Draw{
		CaldronId: pot.Id, Commitment: commitment, Seed: hex.EncodeToString(seed),
		WinnerId: creator, Gifts: []caldron.Gift{}, Total: 0, Payout: 2_500_00,
	}
	saved, err := db.SaveDraw(ctx, draw)
	if err != nil {
		t.Fatalf("сохранение розыгрыша: %v", err)
	}
	if saved.WinnerId != creator {
		t.Errorf("победитель %s, ожидался %s", saved.WinnerId, creator)
	}

	// Розыгрыш бывает один: повтор возвращает уже состоявшийся результат,
	// а не переигрывает исход.
	other := draw
	other.WinnerId = uuid.New()
	again, err := db.SaveDraw(ctx, other)
	if err != nil {
		t.Fatalf("повторное сохранение: %v", err)
	}
	if again.WinnerId != creator {
		t.Errorf("повтор переиграл исход: %s", again.WinnerId)
	}
}

// TestDrawIsImmutable проверяет запрет на правку результата даже мимо
// сервиса: уникальности мало — она не запрещает переписать первый розыгрыш.
func TestDrawIsImmutable(t *testing.T) {
	ctx := context.Background()
	cfg := testsupport.Prepare(t, "caldron")
	db, err := NewDatabase(ctx, cfg)
	if err != nil {
		t.Fatalf("не удалось открыть репозиторий: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	creator := uuid.New()
	pot, err := db.Create(ctx, caldron.Caldron{
		CreatorId: creator, Title: "Юбилей", Type: caldron.TypeGift,
		CreatorParticipates: true, Mode: caldron.ModeFixed, Amount: 2_500_00,
	})
	if err != nil {
		t.Fatalf("создание котла: %v", err)
	}
	seed, commitment, err := db.Seed(ctx, pot.Id)
	if err != nil {
		t.Fatalf("чтение зерна: %v", err)
	}
	if _, err = db.SaveDraw(ctx, caldron.Draw{
		CaldronId: pot.Id, Commitment: commitment, Seed: hex.EncodeToString(seed),
		WinnerId: creator, Gifts: []caldron.Gift{}, Payout: 2_500_00,
	}); err != nil {
		t.Fatalf("сохранение розыгрыша: %v", err)
	}

	raw, err := sql.Open("postgres", cfg.PostgresURL)
	if err != nil {
		t.Fatalf("подключение к базе: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	if _, err = raw.ExecContext(ctx,
		`UPDATE draw SET winner_id = $1 WHERE caldron_id = $2`, uuid.New(), pot.Id); err == nil {
		t.Error("результат розыгрыша удалось переписать")
	}
	if _, err = raw.ExecContext(ctx, `DELETE FROM draw WHERE caldron_id = $1`, pot.Id); err == nil {
		t.Error("результат розыгрыша удалось удалить")
	}
}

func TestSetArbiter(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	creator := uuid.New()
	member := uuid.New()
	pot := createFixed(t, db, creator, 2_500_00)

	updated, err := db.SetArbiter(ctx, pot.Id, &member)
	if err != nil {
		t.Fatalf("назначение арбитра: %v", err)
	}
	if updated.ArbiterId == nil || *updated.ArbiterId != member {
		t.Fatalf("арбитр не назначен: %+v", updated.ArbiterId)
	}
	if !updated.CanDraw(member) || !updated.CanDraw(creator) {
		t.Error("право на розыгрыш определено неверно")
	}

	if _, err = db.Transition(ctx, pot.Id, caldron.StateCancelled, caldron.ActorCreator); err != nil {
		t.Fatalf("отмена: %v", err)
	}
	if _, err = db.SetArbiter(ctx, pot.Id, &creator); !errors.Is(err, caldron.ErrInvalidTransition) {
		t.Errorf("арбитр назначен в отменённом котле: %v", err)
	}
}
