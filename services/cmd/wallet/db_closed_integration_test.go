//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	wallet "wish/middleware/wallet/v1"
	"wish/services/testsupport"

	"github.com/google/uuid"
)

// TestRepositoryReportsBrokenDatabase проверяет свойство, которое иначе
// не проверяется ничем: каждый метод репозитория сообщает о сбое базы,
// а не возвращает пустой результат с nil-ошибкой.
//
// Для кошелька это прямой вопрос денег: молчаливо пустой список кошельков
// или нулевой баланс выглядят как рабочий ответ.
func TestRepositoryReportsBrokenDatabase(t *testing.T) {
	ctx := context.Background()
	db, err := NewDatabaseWallet(ctx, testsupport.Prepare(t, "wallet"))
	if err != nil {
		t.Fatalf("не удалось открыть репозиторий: %v", err)
	}
	// База закрывается намеренно: дальше любой запрос обязан падать.
	if err := db.Close(); err != nil {
		t.Fatalf("закрытие репозитория: %v", err)
	}

	owner := uuid.New()
	walletId := uuid.New()
	params := OperationParams{IdempotencyKey: "op-1", WalletId: walletId, Value: 1000}

	calls := map[string]func() error{
		"Information": func() error {
			return db.Information(ctx, owner, func(*wallet.InformationReply) {})
		},
		"Debit": func() error {
			_, err := db.Debit(ctx, owner, params)
			return err
		},
		"Credit": func() error {
			_, err := db.Credit(ctx, owner, params)
			return err
		},
		"Transfer": func() error {
			_, err := db.Transfer(ctx, owner, TransferParams{
				IdempotencyKey: "tr-1", SourceId: walletId, TargetId: uuid.New(), Value: 1000,
			})
			return err
		},
		"Reserve": func() error {
			_, err := db.Reserve(ctx, owner, ReserveParams{
				IdempotencyKey: "res-1", WalletId: walletId, Value: 1000, TTL: time.Minute,
			})
			return err
		},
		"Confirm": func() error {
			_, err := db.Confirm(ctx, owner, uuid.New())
			return err
		},
		"Reject": func() error {
			_, err := db.Reject(ctx, owner, uuid.New())
			return err
		},
		"ReleaseExpiredReservations": func() error {
			_, err := db.ReleaseExpiredReservations(ctx)
			return err
		},
		"History": func() error {
			_, err := db.History(ctx, owner, walletId, 10, nil)
			return err
		},
		"ChangeState": func() error {
			return db.ChangeState(ctx, owner, walletId, "BLOCKED")
		},
		"EnsurePartitions": func() error {
			_, err := db.EnsurePartitions(ctx, 6)
			return err
		},
		"DefaultPartitionRows": func() error {
			_, err := db.DefaultPartitionRows(ctx)
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
