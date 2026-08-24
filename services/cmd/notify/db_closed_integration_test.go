//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"wish/services/shared/notify"

	"github.com/google/uuid"
)

// TestRepositoryReportsBrokenDatabase проверяет свойство, которое иначе
// не проверяется ничем: каждый метод репозитория сообщает о сбое базы,
// а не возвращает пустой результат с nil-ошибкой.
//
// Для очереди доставки это особенно важно: пустая выборка заданий при сбое
// выглядит как «доставлять нечего», и диспетчер тихо перестаёт работать.
func TestRepositoryReportsBrokenDatabase(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	// База закрывается намеренно: дальше любой запрос обязан падать.
	if err := db.Close(); err != nil {
		t.Fatalf("закрытие репозитория: %v", err)
	}

	id := uuid.New()
	user := uuid.New()
	task := Task{Id: id, EventId: uuid.New(), UserId: user, Channel: notify.ChannelInApp}

	calls := map[string]func() error{
		"Publish": func() error {
			_, _, err := db.Publish(ctx, notify.PublishEvent{
				UserId: user, Type: notify.EventPaymentSettled,
			}, []notify.Channel{notify.ChannelInApp})
			return err
		},
		"EnabledChannels": func() error {
			_, err := db.EnabledChannels(ctx, user, notify.EventPaymentSettled)
			return err
		},
		"Preferences": func() error {
			_, err := db.Preferences(ctx, user)
			return err
		},
		"SetPreference": func() error {
			return db.SetPreference(ctx, user, notify.Preference{
				Type: notify.EventPaymentSettled, Channel: notify.ChannelInApp,
			})
		},
		"Claim": func() error {
			_, err := db.Claim(ctx, 10, time.Minute)
			return err
		},
		"Delivered": func() error {
			return db.Delivered(ctx, id)
		},
		"Retry": func() error {
			return db.Retry(ctx, id, time.Minute, "канал недоступен")
		},
		"Failed": func() error {
			return db.Failed(ctx, id, "канал заблокирован")
		},
		"Defer": func() error {
			return db.Defer(ctx, id, time.Minute)
		},
		"SentSince": func() error {
			_, err := db.SentSince(ctx, user, notify.ChannelInApp, time.Hour)
			return err
		},
		"Unsettled": func() error {
			_, err := db.Unsettled(ctx)
			return err
		},
		"AppendMessage": func() error {
			_, err := db.AppendMessage(ctx, task, "Заголовок", "Текст")
			return err
		},
		"Messages": func() error {
			_, err := db.Messages(ctx, user, 0, 10)
			return err
		},
		"StartMessengerBinding": func() error {
			return db.StartMessengerBinding(ctx, notify.ChannelTelegram, user,
				[]byte("hash"), time.Now().Add(time.Minute))
		},
		"CompleteMessengerBinding": func() error {
			_, err := db.CompleteMessengerBinding(ctx, notify.ChannelTelegram, []byte("hash"), 42)
			return err
		},
		"MessengerBinding": func() error {
			_, err := db.MessengerBinding(ctx, notify.ChannelTelegram, user)
			return err
		},
		"BlockMessenger": func() error {
			return db.BlockMessenger(ctx, notify.ChannelTelegram, user)
		},
		"SetMessengerBlocked": func() error {
			return db.SetMessengerBlocked(ctx, notify.ChannelTelegram, 4242, true)
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
