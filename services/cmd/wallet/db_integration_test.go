//go:build integration

package main

import (
	"context"
	"sync"
	"testing"
	"time"

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

	// Отсоединение — обратная сторона того же обслуживания: вперёд
	// партиции создаются, назад уходят из основной таблицы. Проверяется
	// не сам факт отсоединения, а свойство: данные остаются на месте
	// и доступны для выгрузки.
	t.Run("партиция старше срока хранения отсоединяется, данные остаются", func(t *testing.T) {
		userId := uuid.New()
		var walletId string
		if err := db.Information(ctx, userId, func(reply *wallet.InformationReply) {
			walletId = reply.Id
		}); err != nil {
			t.Fatalf("получение кошелька: %v", err)
		}

		// Партиция позапрошлого года и запись в ней: свежие партиции
		// отсоединять нельзя, а старую взять неоткуда, кроме как создать.
		old := time.Now().AddDate(-2, 0, 0)
		month := time.Date(old.Year(), old.Month(), 1, 0, 0, 0, 0, time.UTC)
		partition := "transaction_" + month.Format("2006_01")
		if _, err := rawDB(t, db).ExecContext(ctx,
			"SELECT fn_ensure_transaction_partition($1)", month); err != nil {
			t.Fatalf("создание старой партиции: %v", err)
		}
		if _, err := rawDB(t, db).ExecContext(ctx,
			"INSERT INTO transaction (target, operation, value, created_at) VALUES ($1, 'DEBIT', 77, $2)",
			walletId, month.AddDate(0, 0, 5)); err != nil {
			t.Fatalf("вставка старой транзакции: %v", err)
		}

		balanceBefore := walletBalance(ctx, t, db, walletId)

		// Срок хранения меньше возраста партиции: год против двух.
		detached, err := db.DetachOldPartitions(ctx, 12)
		if err != nil {
			t.Fatalf("отсоединение: %v", err)
		}
		if detached < 1 {
			t.Fatalf("отсоединено %d партиций, ожидалась хотя бы одна", detached)
		}

		// Данные никуда не делись: таблица осталась в схеме и читается.
		var kept int
		if err = rawDB(t, db).QueryRowContext(ctx,
			"SELECT count(*) FROM "+partition).Scan(&kept); err != nil {
			t.Fatalf("чтение отсоединённой партиции: %v", err)
		}
		if kept != 1 {
			t.Errorf("в отсоединённой партиции %d строк, ожидалась одна", kept)
		}

		// А из основной таблицы ушли: в этом и смысл срока хранения.
		var visible int
		if err = rawDB(t, db).QueryRowContext(ctx,
			"SELECT count(*) FROM transaction WHERE target = $1 AND value = 77", walletId).
			Scan(&visible); err != nil {
			t.Fatalf("чтение основной таблицы: %v", err)
		}
		if visible != 0 {
			t.Errorf("отсоединённые записи всё ещё видны в transaction: %d", visible)
		}

		// Баланс не пересчитывается по истории, поэтому отсоединение
		// не должно его задеть. Иначе архивация превратилась бы
		// в списание денег.
		if after := walletBalance(ctx, t, db, walletId); after != balanceBefore {
			t.Errorf("баланс после отсоединения %d, был %d", after, balanceBefore)
		}

		// Ноль отключает отсоединение: иначе выключить его было бы нечем.
		if detached, err = db.DetachOldPartitions(ctx, 0); err != nil {
			t.Fatalf("отсоединение с нулевым сроком: %v", err)
		}
		if detached != 0 {
			t.Errorf("при нулевом сроке отсоединено %d партиций", detached)
		}

		// Самая старая присоединённая партиция теперь не старше срока.
		oldest, err := db.OldestPartition(ctx)
		if err != nil {
			t.Fatalf("возраст партиции: %v", err)
		}
		if oldest.IsZero() {
			t.Fatal("партиций не осталось вовсе")
		}
		if !oldest.After(month) {
			t.Errorf("самая старая партиция %s, ожидалась новее %s",
				oldest.Format("2006-01"), month.Format("2006-01"))
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

// walletBalance читает баланс кошелька прямым запросом: через отчёт он
// считается вместе с резервами, а здесь нужно именно сохранённое значение.
func walletBalance(ctx context.Context, t *testing.T, db DatabaseWallet, walletId string) int64 {
	t.Helper()
	var balance int64
	if err := rawDB(t, db).QueryRowContext(ctx,
		"SELECT balance FROM wallet WHERE id = $1", walletId).Scan(&balance); err != nil {
		t.Fatalf("чтение баланса: %v", err)
	}
	return balance
}
