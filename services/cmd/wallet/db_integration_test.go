//go:build integration

package main

import (
	"context"
	"sync"
	"testing"

	"wish/middleware/wallet"
	"wish/services/testsupport"

	"github.com/google/uuid"
)

func TestWalletRepository(t *testing.T) {
	ctx := context.Background()
	db, err := NewDatabaseWallet(ctx, testsupport.Prepare(t, "wallet"))
	if err != nil {
		t.Fatalf("не удалось открыть репозиторий: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	collect := func(t *testing.T, userId uuid.UUID) []*wallet.InformationReply {
		t.Helper()
		var replies []*wallet.InformationReply
		if err := db.Information(ctx, userId, func(reply *wallet.InformationReply) {
			replies = append(replies, reply)
		}); err != nil {
			t.Fatalf("чтение кошельков: %v", err)
		}
		return replies
	}

	t.Run("кошелёк создаётся при первом обращении", func(t *testing.T) {
		userId := uuid.New()
		replies := collect(t, userId)
		if len(replies) != 1 {
			t.Fatalf("кошельков %d, ожидался 1", len(replies))
		}
		if replies[0].Balance != 0 {
			t.Errorf("баланс = %d, ожидался 0", replies[0].Balance)
		}
		if replies[0].State != wallet.WalletState_ACTIVE {
			t.Errorf("состояние = %s, ожидалось ACTIVE", replies[0].State)
		}
	})

	t.Run("повторное обращение не создаёт второй кошелёк", func(t *testing.T) {
		userId := uuid.New()
		collect(t, userId)
		if replies := collect(t, userId); len(replies) != 1 {
			t.Fatalf("кошельков %d, ожидался 1", len(replies))
		}
	})

	t.Run("транзакции одного номинала не размножают кошелёк", func(t *testing.T) {
		userId := uuid.New()
		walletId := collect(t, userId)[0].Id

		// GROUP BY по сумме разбивал агрегат по номиналу, и кошелёк
		// возвращался несколько раз с частичными суммами.
		for range 3 {
			if _, err := rawDB(t, db).ExecContext(ctx,
				"INSERT INTO transaction (target, operation, value) VALUES ($1, 'DEBIT', 100)",
				walletId); err != nil {
				t.Fatalf("вставка транзакции: %v", err)
			}
		}

		replies := collect(t, userId)
		if len(replies) != 1 {
			t.Fatalf("кошельков %d, ожидался 1", len(replies))
		}
		if replies[0].Transactions != 3 {
			t.Errorf("транзакций %d, ожидалось 3", replies[0].Transactions)
		}
		if replies[0].ReservedDebit != 300 {
			t.Errorf("резерв на зачисление = %d, ожидалось 300", replies[0].ReservedDebit)
		}
	})

	t.Run("чужие транзакции не попадают в отчёт", func(t *testing.T) {
		owner := uuid.New()
		stranger := uuid.New()
		ownerWallet := collect(t, owner)[0].Id
		strangerWallet := collect(t, stranger)[0].Id

		if _, err := rawDB(t, db).ExecContext(ctx,
			"INSERT INTO transaction (target, operation, value) VALUES ($1, 'DEBIT', 999)",
			strangerWallet); err != nil {
			t.Fatalf("вставка транзакции: %v", err)
		}

		replies := collect(t, owner)
		if replies[0].Id != ownerWallet {
			t.Fatalf("вернулся чужой кошелёк %s", replies[0].Id)
		}
		if replies[0].ReservedDebit != 0 {
			t.Errorf("резерв = %d, ожидался 0: чужая транзакция попала в отчёт", replies[0].ReservedDebit)
		}
	})

	t.Run("успешная транзакция меняет баланс", func(t *testing.T) {
		userId := uuid.New()
		walletId := collect(t, userId)[0].Id

		if _, err := rawDB(t, db).ExecContext(ctx,
			"INSERT INTO transaction (target, operation, value) VALUES ($1, 'DEBIT', 500)",
			walletId); err != nil {
			t.Fatalf("вставка транзакции: %v", err)
		}
		if _, err := rawDB(t, db).ExecContext(ctx,
			"UPDATE transaction SET state = 'SUCCESS' WHERE target = $1", walletId); err != nil {
			t.Fatalf("подтверждение транзакции: %v", err)
		}

		if balance := collect(t, userId)[0].Balance; balance != 500 {
			t.Errorf("баланс = %d, ожидалось 500", balance)
		}
	})

	t.Run("параллельные обращения не ломают автосоздание", func(t *testing.T) {
		userId := uuid.New()
		const workers = 8

		// Прежняя схема «нет строк -> INSERT -> goto» нарушала
		// UNIQUE (user_id, type) при параллельных запросах.
		var wg sync.WaitGroup
		errs := make([]error, workers)
		for i := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = db.Information(ctx, userId, func(*wallet.InformationReply) {})
			}()
		}
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("запрос %d завершился ошибкой: %v", i, err)
			}
		}
		if replies := collect(t, userId); len(replies) != 1 {
			t.Fatalf("кошельков %d, ожидался 1", len(replies))
		}
	})
}
