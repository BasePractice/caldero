//go:build integration

package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"wish/services/shared/marketplace"
	"wish/services/shared/wishlist"
	"wish/services/testsupport"

	"github.com/google/uuid"
)

func newTestDatabase(t *testing.T) Database {
	t.Helper()
	db, err := NewDatabase(context.Background(), testsupport.Prepare(t, "wishlist"))
	if err != nil {
		t.Fatalf("не удалось открыть репозиторий: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func createProduct(t *testing.T, db Database, owner uuid.UUID) wishlist.Item {
	t.Helper()
	now := time.Now()
	item, err := db.Create(context.Background(), wishlist.Item{
		UserId:    owner,
		Kind:      wishlist.KindProduct,
		State:     wishlist.StateVisible,
		Priority:  1,
		Title:     "Кофеварка",
		Provider:  marketplace.ProviderStub,
		ProductId: "coffee-machine",
		URL:       "https://example.invalid/product/coffee-machine",
		Price:     12_000_00,
		PriceAt:   &now,
	})
	if err != nil {
		t.Fatalf("создание элемента: %v", err)
	}
	return item
}

func TestCreateAndRead(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	owner := uuid.New()

	item := createProduct(t, db, owner)
	if item.Price != 12_000_00 || item.PriceAt == nil {
		t.Errorf("цена не сохранена: %+v", item)
	}

	money, err := db.Create(ctx, wishlist.Item{
		UserId: owner, Kind: wishlist.KindMoney, State: wishlist.StateVisible,
		Priority: 2, Title: "На велосипед", Amount: 30_000_00,
	})
	if err != nil {
		t.Fatalf("создание денежного элемента: %v", err)
	}
	if money.Amount != 30_000_00 {
		t.Errorf("сумма не сохранена: %s", money.Amount)
	}

	items, err := db.Items(ctx, owner, nil)
	if err != nil {
		t.Fatalf("чтение списка: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("элементов %d, ожидалось 2", len(items))
	}
	// Сортировка по приоритету: важное сверху.
	if items[0].Id != item.Id {
		t.Errorf("порядок элементов не по приоритету: %+v", items)
	}

	visible, err := db.Items(ctx, owner, []wishlist.State{wishlist.StateVisible})
	if err != nil {
		t.Fatalf("чтение видимых: %v", err)
	}
	if len(visible) != 2 {
		t.Errorf("видимых элементов %d, ожидалось 2", len(visible))
	}
}

// TestSchemaRejectsInconsistentItem проверяет, что схема не даст записать
// элемент без того, из чего он состоит, даже мимо сервиса.
func TestSchemaRejectsInconsistentItem(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	owner := uuid.New()

	if _, err := db.Create(ctx, wishlist.Item{
		UserId: owner, Kind: wishlist.KindProduct, State: wishlist.StateVisible,
		Priority: 1, Title: "Без площадки",
	}); err == nil {
		t.Error("товар без площадки записан")
	}

	if _, err := db.Create(ctx, wishlist.Item{
		UserId: owner, Kind: wishlist.KindMoney, State: wishlist.StateVisible,
		Priority: 1, Title: "Без суммы",
	}); err == nil {
		t.Error("денежный элемент без суммы записан")
	}
}

// TestConcurrentReservation — главный тест задачи: два дарителя,
// одновременно выбравшие один подарок, не могут зарезервировать его оба.
func TestConcurrentReservation(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	owner := uuid.New()
	item := createProduct(t, db, owner)

	const givers = 8
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded []uuid.UUID
	)
	until := time.Now().Add(time.Hour)
	for range givers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			giver := uuid.New()
			if _, err := db.Transition(ctx, item.Id, Transition{
				Actor:         wishlist.ActorGiver,
				To:            wishlist.StateChosen,
				Giver:         &giver,
				ReservedUntil: &until,
			}); err == nil {
				mu.Lock()
				succeeded = append(succeeded, giver)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(succeeded) != 1 {
		t.Fatalf("подарок зарезервировали %d дарителей, ожидался один", len(succeeded))
	}
	current, err := db.Item(ctx, item.Id)
	if err != nil {
		t.Fatalf("чтение элемента: %v", err)
	}
	if current.GiverId == nil || *current.GiverId != succeeded[0] {
		t.Errorf("резерв достался не тому, кто успел первым: %+v", current)
	}
}

func TestTransitionChecksActorAndReserver(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	owner := uuid.New()
	giver := uuid.New()
	stranger := uuid.New()
	item := createProduct(t, db, owner)

	until := time.Now().Add(time.Hour)
	if _, err := db.Transition(ctx, item.Id, Transition{
		Actor: wishlist.ActorGiver, To: wishlist.StateChosen,
		Giver: &giver, ReservedUntil: &until,
	}); err != nil {
		t.Fatalf("резервирование: %v", err)
	}

	t.Run("чужой даритель не снимает резерв", func(t *testing.T) {
		_, err := db.Transition(ctx, item.Id, Transition{
			Actor: wishlist.ActorGiver, To: wishlist.StateVisible, Giver: &stranger,
		})
		if !errors.Is(err, wishlist.ErrForbiddenTransition) {
			t.Errorf("получено %v, ожидалась %v", err, wishlist.ErrForbiddenTransition)
		}
	})

	t.Run("даритель не подтверждает за одаряемого", func(t *testing.T) {
		_, err := db.Transition(ctx, item.Id, Transition{
			Actor: wishlist.ActorGiver, To: wishlist.StateConfirmed, Giver: &giver,
		})
		if !errors.Is(err, wishlist.ErrForbiddenTransition) {
			t.Errorf("получено %v, ожидалась %v", err, wishlist.ErrForbiddenTransition)
		}
	})

	t.Run("акцепт из выбранного состояния невозможен", func(t *testing.T) {
		_, err := db.Transition(ctx, item.Id, Transition{
			Actor: wishlist.ActorGiver, To: wishlist.StateAccepted, Giver: &giver,
		})
		if !errors.Is(err, wishlist.ErrInvalidTransition) {
			t.Errorf("получено %v, ожидалась %v", err, wishlist.ErrInvalidTransition)
		}
	})
}

// TestFullScenario проходит сквозной сценарий README: даритель выбирает,
// одаряемый подтверждает, даритель акцептует.
func TestFullScenario(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	owner := uuid.New()
	giver := uuid.New()
	item := createProduct(t, db, owner)
	until := time.Now().Add(time.Hour)

	chosen, err := db.Transition(ctx, item.Id, Transition{
		Actor: wishlist.ActorGiver, To: wishlist.StateChosen,
		Giver: &giver, ReservedUntil: &until,
	})
	if err != nil {
		t.Fatalf("резервирование: %v", err)
	}
	if chosen.ReservedUntil == nil {
		t.Error("срок резерва не сохранён")
	}

	// Пока подарок выбран, другим дарителям он не показывается.
	visible, err := db.Items(ctx, owner, []wishlist.State{wishlist.StateVisible})
	if err != nil {
		t.Fatalf("чтение видимых: %v", err)
	}
	if len(visible) != 0 {
		t.Errorf("выбранный подарок остался виден другим: %+v", visible)
	}

	confirmed, err := db.Transition(ctx, item.Id, Transition{
		Actor: wishlist.ActorOwner, To: wishlist.StateConfirmed,
	})
	if err != nil {
		t.Fatalf("подтверждение: %v", err)
	}
	// Срок резерва снимается: торопить дарителя после подтверждения нечем.
	if confirmed.ReservedUntil != nil {
		t.Errorf("срок резерва остался после подтверждения: %v", confirmed.ReservedUntil)
	}
	if confirmed.GiverId == nil || *confirmed.GiverId != giver {
		t.Errorf("даритель потерян при подтверждении: %+v", confirmed)
	}

	accepted, err := db.Transition(ctx, item.Id, Transition{
		Actor: wishlist.ActorGiver, To: wishlist.StateAccepted,
		Giver: &giver, OrderId: "stub-order-1",
	})
	if err != nil {
		t.Fatalf("акцепт: %v", err)
	}
	if accepted.OrderId != "stub-order-1" {
		t.Errorf("номер заказа не сохранён: %q", accepted.OrderId)
	}

	chosenByGiver, err := db.Chosen(ctx, giver)
	if err != nil {
		t.Fatalf("чтение выбранного дарителем: %v", err)
	}
	if len(chosenByGiver) != 1 {
		t.Errorf("даритель видит %d своих подарков, ожидался один", len(chosenByGiver))
	}
}

func TestReleaseExpired(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	owner := uuid.New()
	giver := uuid.New()

	expired := createProduct(t, db, owner)
	fresh := createProduct(t, db, owner)

	past := time.Now().Add(-time.Minute)
	future := time.Now().Add(time.Hour)
	if _, err := db.Transition(ctx, expired.Id, Transition{
		Actor: wishlist.ActorGiver, To: wishlist.StateChosen,
		Giver: &giver, ReservedUntil: &past,
	}); err != nil {
		t.Fatalf("резервирование просроченного: %v", err)
	}
	if _, err := db.Transition(ctx, fresh.Id, Transition{
		Actor: wishlist.ActorGiver, To: wishlist.StateChosen,
		Giver: &giver, ReservedUntil: &future,
	}); err != nil {
		t.Fatalf("резервирование свежего: %v", err)
	}

	released, err := db.ReleaseExpired(ctx)
	if err != nil {
		t.Fatalf("освобождение резервов: %v", err)
	}
	if len(released) != 1 || released[0].Id != expired.Id {
		t.Fatalf("освобождено %d резервов, ожидался один просроченный", len(released))
	}

	current, err := db.Item(ctx, fresh.Id)
	if err != nil {
		t.Fatalf("чтение элемента: %v", err)
	}
	if current.State != wishlist.StateChosen {
		t.Error("действующий резерв снят вместе с просроченным")
	}
}

func TestDeleteOnlyUnchosen(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	owner := uuid.New()
	giver := uuid.New()
	item := createProduct(t, db, owner)

	t.Run("чужой элемент не удаляется", func(t *testing.T) {
		if err := db.Delete(ctx, item.Id, uuid.New()); !errors.Is(err, ErrNotFound) {
			t.Errorf("получено %v, ожидалась %v", err, ErrNotFound)
		}
	})

	until := time.Now().Add(time.Hour)
	if _, err := db.Transition(ctx, item.Id, Transition{
		Actor: wishlist.ActorGiver, To: wishlist.StateChosen,
		Giver: &giver, ReservedUntil: &until,
	}); err != nil {
		t.Fatalf("резервирование: %v", err)
	}

	t.Run("выбранный элемент не удаляется", func(t *testing.T) {
		// Иначе даритель узнал бы об отказе исчезновением подарка,
		// а не оповещением.
		if err := db.Delete(ctx, item.Id, owner); !errors.Is(err, ErrNotFound) {
			t.Errorf("получено %v, ожидалась %v", err, ErrNotFound)
		}
	})

	free := createProduct(t, db, owner)
	if err := db.Delete(ctx, free.Id, owner); err != nil {
		t.Errorf("невыбранный элемент не удалился: %v", err)
	}
}
