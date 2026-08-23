//go:build integration

package main

import (
	"context"
	"errors"
	"sync"
	"testing"

	"wish/middleware/wallet"
	"wish/services/testsupport"

	"github.com/google/uuid"
)

func TestWalletOperations(t *testing.T) {
	ctx := context.Background()
	db, err := NewDatabaseWallet(ctx, testsupport.Prepare(t, "wallet"))
	if err != nil {
		t.Fatalf("не удалось открыть репозиторий: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	walletOf := func(t *testing.T, owner uuid.UUID) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := db.Information(ctx, owner, func(reply *wallet.InformationReply) {
			id = uuid.MustParse(reply.Id)
		}); err != nil {
			t.Fatalf("получение кошелька: %v", err)
		}
		return id
	}
	balanceOf := func(t *testing.T, owner uuid.UUID) int64 {
		t.Helper()
		var balance int64
		if err := db.Information(ctx, owner, func(reply *wallet.InformationReply) {
			balance = reply.Balance
		}); err != nil {
			t.Fatalf("чтение баланса: %v", err)
		}
		return balance
	}

	t.Run("зачисление увеличивает баланс", func(t *testing.T) {
		owner := uuid.New()
		walletOf(t, owner)

		result, err := db.Debit(ctx, owner, OperationParams{
			IdempotencyKey: uuid.NewString(), Value: 1000, Message: "пополнение",
		})
		if err != nil {
			t.Fatalf("зачисление: %v", err)
		}
		if result.Balance != 1000 {
			t.Errorf("баланс в ответе %d, ожидался 1000", result.Balance)
		}
		if got := balanceOf(t, owner); got != 1000 {
			t.Errorf("баланс кошелька %d, ожидался 1000", got)
		}
	})

	t.Run("списание больше баланса отклоняется", func(t *testing.T) {
		owner := uuid.New()
		walletOf(t, owner)
		if _, err := db.Debit(ctx, owner, OperationParams{IdempotencyKey: uuid.NewString(), Value: 500}); err != nil {
			t.Fatalf("зачисление: %v", err)
		}

		_, err := db.Credit(ctx, owner, OperationParams{IdempotencyKey: uuid.NewString(), Value: 501})
		if !errors.Is(err, ErrInsufficientBalance) {
			t.Fatalf("получено %v, ожидалась ErrInsufficientBalance", err)
		}
		if got := balanceOf(t, owner); got != 500 {
			t.Errorf("баланс изменился на %d, а не должен был", got)
		}
	})

	t.Run("повтор с тем же ключом не проводит операцию дважды", func(t *testing.T) {
		owner := uuid.New()
		walletOf(t, owner)
		key := uuid.NewString()

		first, err := db.Debit(ctx, owner, OperationParams{IdempotencyKey: key, Value: 700})
		if err != nil {
			t.Fatalf("первое зачисление: %v", err)
		}
		second, err := db.Debit(ctx, owner, OperationParams{IdempotencyKey: key, Value: 700})
		if err != nil {
			t.Fatalf("повтор: %v", err)
		}

		if !second.Idempotent {
			t.Error("повтор не помечен как идемпотентный")
		}
		if second.Id != first.Id {
			t.Errorf("повтор создал новую транзакцию %s вместо %s", second.Id, first.Id)
		}
		if got := balanceOf(t, owner); got != 700 {
			t.Errorf("баланс %d, ожидался 700: повтор провёл операцию дважды", got)
		}
	})

	t.Run("перевод переносит средства между кошельками", func(t *testing.T) {
		owner := uuid.New()
		source := walletOf(t, owner)

		other := uuid.New()
		target := walletOf(t, other)

		if _, err := db.Debit(ctx, owner, OperationParams{IdempotencyKey: uuid.NewString(), Value: 1000}); err != nil {
			t.Fatalf("пополнение: %v", err)
		}

		if _, err := db.Transfer(ctx, owner, TransferParams{
			IdempotencyKey: uuid.NewString(), SourceId: source, TargetId: target, Value: 400,
		}); err != nil {
			t.Fatalf("перевод: %v", err)
		}

		if got := balanceOf(t, owner); got != 600 {
			t.Errorf("баланс отправителя %d, ожидался 600", got)
		}
		if got := balanceOf(t, other); got != 400 {
			t.Errorf("баланс получателя %d, ожидался 400", got)
		}
	})

	t.Run("перевод с чужого кошелька отклоняется", func(t *testing.T) {
		owner := uuid.New()
		source := walletOf(t, owner)
		stranger := uuid.New()
		target := walletOf(t, stranger)

		if _, err := db.Debit(ctx, owner, OperationParams{IdempotencyKey: uuid.NewString(), Value: 100}); err != nil {
			t.Fatalf("пополнение: %v", err)
		}

		_, err := db.Transfer(ctx, stranger, TransferParams{
			IdempotencyKey: uuid.NewString(), SourceId: source, TargetId: target, Value: 50,
		})
		if !errors.Is(err, ErrWalletNotFound) {
			t.Fatalf("получено %v, ожидалась ErrWalletNotFound", err)
		}
	})

	t.Run("параллельные списания не теряются", func(t *testing.T) {
		owner := uuid.New()
		walletOf(t, owner)
		if _, err := db.Debit(ctx, owner, OperationParams{IdempotencyKey: uuid.NewString(), Value: 1000}); err != nil {
			t.Fatalf("пополнение: %v", err)
		}

		// Без блокировки строки два одновременных списания прочитали бы
		// один и тот же баланс, и одно из них потерялось бы.
		const workers = 10
		var wg sync.WaitGroup
		errs := make([]error, workers)
		for i := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = db.Credit(ctx, owner, OperationParams{
					IdempotencyKey: uuid.NewString(), Value: 100,
				})
			}()
		}
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("списание %d: %v", i, err)
			}
		}
		if got := balanceOf(t, owner); got != 0 {
			t.Errorf("баланс %d, ожидался 0: часть списаний потерялась", got)
		}
	})

	t.Run("история отдаётся постранично", func(t *testing.T) {
		owner := uuid.New()
		walletOf(t, owner)
		for range 5 {
			if _, err := db.Debit(ctx, owner, OperationParams{IdempotencyKey: uuid.NewString(), Value: 10}); err != nil {
				t.Fatalf("зачисление: %v", err)
			}
		}

		page, err := db.History(ctx, owner, uuid.Nil, 3, nil)
		if err != nil {
			t.Fatalf("история: %v", err)
		}
		if len(page) != 3 {
			t.Fatalf("на странице %d операций, ожидалось 3", len(page))
		}

		cursor := page[len(page)-1].CreatedAt
		next, err := db.History(ctx, owner, uuid.Nil, 3, &cursor)
		if err != nil {
			t.Fatalf("вторая страница: %v", err)
		}
		if len(next) != 2 {
			t.Fatalf("на второй странице %d операций, ожидалось 2", len(next))
		}
		for _, t2 := range next {
			if !t2.CreatedAt.Before(cursor) {
				t.Error("вторая страница содержит операции из первой")
			}
		}
	})

	t.Run("операции с заблокированным кошельком отклоняются", func(t *testing.T) {
		owner := uuid.New()
		walletId := walletOf(t, owner)

		if err := db.ChangeState(ctx, owner, walletId, "BLOCKED"); err != nil {
			t.Fatalf("блокировка: %v", err)
		}
		_, err := db.Debit(ctx, owner, OperationParams{IdempotencyKey: uuid.NewString(), Value: 10})
		if !errors.Is(err, ErrWalletNotActive) {
			t.Fatalf("получено %v, ожидалась ErrWalletNotActive", err)
		}

		if err = db.ChangeState(ctx, owner, walletId, "ACTIVE"); err != nil {
			t.Fatalf("разблокировка: %v", err)
		}
		if _, err = db.Debit(ctx, owner, OperationParams{IdempotencyKey: uuid.NewString(), Value: 10}); err != nil {
			t.Fatalf("после разблокировки: %v", err)
		}
	})

	t.Run("удалённый кошелёк нельзя вернуть в работу", func(t *testing.T) {
		owner := uuid.New()
		walletId := walletOf(t, owner)

		if err := db.ChangeState(ctx, owner, walletId, "DELETED"); err != nil {
			t.Fatalf("удаление: %v", err)
		}
		// DELETED терминально: переход из него запрещён триггером.
		if err := db.ChangeState(ctx, owner, walletId, "ACTIVE"); err == nil {
			t.Fatal("переход из DELETED должен быть запрещён")
		}
	})
}
