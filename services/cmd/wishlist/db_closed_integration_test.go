//go:build integration

package main

import (
	"context"
	"testing"

	"wish/services/shared/credit"
	"wish/services/shared/wishlist"
	"wish/services/testsupport"

	"github.com/google/uuid"
)

// TestRepositoryReportsBrokenDatabase проверяет свойство, которое иначе
// не проверяется ничем: каждый метод репозитория сообщает о сбое базы,
// а не возвращает пустой результат с nil-ошибкой.
//
// Молчаливый пустой ответ здесь опаснее отказа: пользователь увидел бы
// пустой список желаний вместо сообщения о сбое и решил бы, что список
// потерян.
func TestRepositoryReportsBrokenDatabase(t *testing.T) {
	ctx := context.Background()
	db, err := NewDatabase(ctx, testsupport.Prepare(t, "wishlist"))
	if err != nil {
		t.Fatalf("не удалось открыть репозиторий: %v", err)
	}
	// База закрывается намеренно: дальше любой запрос обязан падать.
	if err := db.Close(); err != nil {
		t.Fatalf("закрытие репозитория: %v", err)
	}

	id := uuid.New()
	user := uuid.New()

	calls := map[string]func() error{
		"Create": func() error {
			_, err := db.Create(ctx, wishlist.Item{
				Id: id, UserId: user, Kind: wishlist.KindMoney, State: wishlist.StateVisible,
			})
			return err
		},
		"Items": func() error {
			_, err := db.Items(ctx, user, []wishlist.State{wishlist.StateVisible})
			return err
		},
		"Chosen": func() error {
			_, err := db.Chosen(ctx, user)
			return err
		},
		"Item": func() error {
			_, err := db.Item(ctx, id)
			return err
		},
		"Delete": func() error {
			return db.Delete(ctx, id, user)
		},
		"Transition": func() error {
			_, err := db.Transition(ctx, id, Transition{
				Actor: wishlist.ActorOwner, To: wishlist.StateHidden,
			})
			return err
		},
		"ReleaseExpired": func() error {
			_, err := db.ReleaseExpired(ctx)
			return err
		},
		"StartRun": func() error {
			_, err := db.StartRun(ctx, user, credit.Amount(1000), []byte("seed"))
			return err
		},
		"AddPurchase": func() error {
			_, err := db.AddPurchase(ctx, wishlist.Purchase{RunId: id})
			return err
		},
		"SettlePurchase": func() error {
			return db.SettlePurchase(ctx, id, true, true, "order-1", "")
		},
		"FinishRun": func() error {
			_, err := db.FinishRun(ctx, id, credit.Amount(1000), wishlist.RunDone)
			return err
		},
		"Run": func() error {
			_, err := db.Run(ctx, id)
			return err
		},
		"Runs": func() error {
			_, err := db.Runs(ctx, user, 10)
			return err
		},
		"Ping": func() error {
			return db.Ping(ctx)
		},
	}

	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil {
				t.Error("сбой базы не превратился в ошибку")
			}
		})
	}

	if db.Stats().MaxOpenConnections == 0 {
		t.Error("статистика пула не заполнена")
	}
}
