//go:build integration

package main

import (
	"context"
	"testing"

	"wish/services/shared/caldron"
	"wish/services/shared/credit"
	"wish/services/testsupport"

	"github.com/google/uuid"
)

// TestRepositoryReportsBrokenDatabase проверяет свойство, которое иначе
// не проверяется ничем: каждый метод репозитория сообщает о сбое базы,
// а не возвращает пустой результат с nil-ошибкой.
//
// Молчаливый пустой ответ здесь опаснее отказа: обработчик отдал бы
// пользователю котёл без участников или пустой список подарков и выглядел
// бы исправным.
func TestRepositoryReportsBrokenDatabase(t *testing.T) {
	ctx := context.Background()
	db, err := NewDatabase(ctx, testsupport.Prepare(t, "caldron"))
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
			_, err := db.Create(ctx, caldron.Caldron{
				Id: id, CreatorId: user, Type: caldron.TypeGift, Mode: caldron.ModeFixed,
			})
			return err
		},
		"Caldron": func() error {
			_, err := db.Caldron(ctx, id)
			return err
		},
		"ByUser": func() error {
			_, err := db.ByUser(ctx, user)
			return err
		},
		"AddParticipant": func() error {
			_, err := db.AddParticipant(ctx, id, caldron.AddParticipant{UserId: user})
			return err
		},
		"RemoveParticipant": func() error {
			_, err := db.RemoveParticipant(ctx, id, user)
			return err
		},
		"SetWallet": func() error {
			return db.SetWallet(ctx, id, uuid.New())
		},
		"StartContribution": func() error {
			_, _, err := db.StartContribution(ctx, id, user, credit.Amount(1000))
			return err
		},
		"MarkPaid": func() error {
			_, err := db.MarkPaid(ctx, id, user, credit.Amount(1000))
			return err
		},
		"MarkRefunded": func() error {
			return db.MarkRefunded(ctx, id, user)
		},
		"Transition": func() error {
			_, err := db.Transition(ctx, id, caldron.StateCancelled, caldron.ActorCreator)
			return err
		},
		"PendingRefunds": func() error {
			_, err := db.PendingRefunds(ctx, 10)
			return err
		},
		"SetArbiter": func() error {
			_, err := db.SetArbiter(ctx, id, &user)
			return err
		},
		"ReplaceGifts": func() error {
			_, err := db.ReplaceGifts(ctx, id, user, nil)
			return err
		},
		"Gifts": func() error {
			_, err := db.Gifts(ctx, id, &user)
			return err
		},
		"Seed": func() error {
			_, _, err := db.Seed(ctx, id)
			return err
		},
		"SaveDraw": func() error {
			_, err := db.SaveDraw(ctx, caldron.Draw{CaldronId: id, WinnerId: user})
			return err
		},
		"Draw": func() error {
			_, err := db.Draw(ctx, id)
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

	// Статистика пула остаётся доступной и после закрытия: метрики
	// собираются в том числе во время остановки сервиса.
	if db.Stats().MaxOpenConnections == 0 {
		t.Error("статистика пула не заполнена")
	}
}
