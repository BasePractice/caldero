//go:build integration

package main

import (
	"context"
	"sync"
	"testing"

	wallet "wish/middleware/wallet/v1"
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

func TestPartitionMaintenance(t *testing.T) {
	ctx := context.Background()
	db, err := NewDatabaseWallet(ctx, testsupport.Prepare(t, "wallet"))
	if err != nil {
		t.Fatalf("не удалось открыть репозиторий: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	partitions := func(t *testing.T) int {
		t.Helper()
		var count int
		if err := rawDB(t, db).QueryRowContext(ctx,
			"SELECT count(*) FROM pg_class WHERE relname LIKE 'transaction_2%' AND relkind = 'r'").
			Scan(&count); err != nil {
			t.Fatalf("подсчёт партиций: %v", err)
		}
		return count
	}

	t.Run("создаёт недостающие партиции", func(t *testing.T) {
		before := partitions(t)

		// Миграция создаёт окно на два года вперёд, поэтому на 6 месяцев
		// вперёд создавать нечего.
		created, err := db.EnsurePartitions(ctx, 6)
		if err != nil {
			t.Fatalf("создание партиций: %v", err)
		}
		if created != 0 {
			t.Errorf("создано %d партиций, ожидалось 0: окно ещё не кончилось", created)
		}

		// А за пределами окна — уже есть что создавать.
		created, err = db.EnsurePartitions(ctx, 40)
		if err != nil {
			t.Fatalf("создание партиций: %v", err)
		}
		if created == 0 {
			t.Fatal("за пределами окна партиции должны создаваться")
		}
		if after := partitions(t); after != before+created {
			t.Errorf("партиций стало %d, ожидалось %d", after, before+created)
		}
	})

	t.Run("повторный вызов ничего не создаёт", func(t *testing.T) {
		if _, err := db.EnsurePartitions(ctx, 40); err != nil {
			t.Fatalf("первый вызов: %v", err)
		}
		created, err := db.EnsurePartitions(ctx, 40)
		if err != nil {
			t.Fatalf("повторный вызов: %v", err)
		}
		if created != 0 {
			t.Errorf("создано %d партиций, ожидалось 0", created)
		}
	})

	t.Run("партиция по умолчанию пуста, пока окно не кончилось", func(t *testing.T) {
		userId := uuid.New()
		var walletId string
		if err := db.Information(ctx, userId, func(reply *wallet.InformationReply) {
			walletId = reply.Id
		}); err != nil {
			t.Fatalf("получение кошелька: %v", err)
		}
		if _, err := rawDB(t, db).ExecContext(ctx,
			"INSERT INTO transaction (target, operation, value) VALUES ($1, 'DEBIT', 10)",
			walletId); err != nil {
			t.Fatalf("вставка транзакции: %v", err)
		}

		rows, err := db.DefaultPartitionRows(ctx)
		if err != nil {
			t.Fatalf("подсчёт: %v", err)
		}
		if rows != 0 {
			t.Errorf("в партиции по умолчанию %d строк, ожидалось 0", rows)
		}
	})
}
