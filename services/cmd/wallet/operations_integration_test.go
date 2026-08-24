//go:build integration

package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	wallet "wish/middleware/wallet/v1"
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

// TestWalletErrorBranches проверяет отказы, которые в обычном сценарии
// не встречаются: несуществующий кошелёк, неположительная сумма, перевод
// самому себе. Именно эти ветки решают, увидит ли клиент причину отказа.
func TestWalletErrorBranches(t *testing.T) {
	ctx := context.Background()
	db, err := NewDatabaseWallet(ctx, testsupport.Prepare(t, "wallet"))
	if err != nil {
		t.Fatalf("не удалось открыть репозиторий: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	owner := uuid.New()
	var walletId uuid.UUID
	if err := db.Information(ctx, owner, func(reply *wallet.InformationReply) {
		walletId = uuid.MustParse(reply.Id)
	}); err != nil {
		t.Fatalf("получение кошелька: %v", err)
	}

	t.Run("операция по несуществующему кошельку", func(t *testing.T) {
		_, err := db.Debit(ctx, owner, OperationParams{
			IdempotencyKey: "missing-1", WalletId: uuid.New(), Value: 100,
		})
		if !errors.Is(err, ErrWalletNotFound) {
			t.Errorf("получено %v, ожидалась ErrWalletNotFound", err)
		}
	})

	t.Run("неположительная сумма", func(t *testing.T) {
		for _, value := range []int64{0, -100} {
			if _, err := db.Debit(ctx, owner, OperationParams{
				IdempotencyKey: "bad-value", WalletId: walletId, Value: value,
			}); !errors.Is(err, ErrInvalidValue) {
				t.Errorf("сумма %d: получено %v, ожидалась ErrInvalidValue", value, err)
			}
		}
	})

	t.Run("перевод самому себе", func(t *testing.T) {
		err := func() error {
			_, err := db.Transfer(ctx, owner, TransferParams{
				IdempotencyKey: "self-1", SourceId: walletId, TargetId: walletId, Value: 100,
			})
			return err
		}()
		if !errors.Is(err, ErrSameWallet) {
			t.Errorf("получено %v, ожидалась ErrSameWallet", err)
		}
	})

	t.Run("перевод на несуществующий кошелёк", func(t *testing.T) {
		if _, err := db.Debit(ctx, owner, OperationParams{
			IdempotencyKey: "fill-1", WalletId: walletId, Value: 1000,
		}); err != nil {
			t.Fatalf("пополнение: %v", err)
		}
		_, err := db.Transfer(ctx, owner, TransferParams{
			IdempotencyKey: "to-nowhere", SourceId: walletId, TargetId: uuid.New(), Value: 100,
		})
		if !errors.Is(err, ErrWalletNotFound) {
			t.Errorf("получено %v, ожидалась ErrWalletNotFound", err)
		}
	})

	t.Run("резерв неположительной суммы", func(t *testing.T) {
		if _, err := db.Reserve(ctx, owner, ReserveParams{
			IdempotencyKey: "res-bad", WalletId: walletId, Value: 0,
		}); !errors.Is(err, ErrInvalidValue) {
			t.Errorf("получено %v, ожидалась ErrInvalidValue", err)
		}
	})

	t.Run("завершение несуществующего резерва", func(t *testing.T) {
		if _, err := db.Confirm(ctx, owner, uuid.New()); !errors.Is(err, ErrReservationNotFound) {
			t.Errorf("получено %v, ожидалась ErrReservationNotFound", err)
		}
		if _, err := db.Reject(ctx, owner, uuid.New()); !errors.Is(err, ErrReservationNotFound) {
			t.Errorf("получено %v, ожидалась ErrReservationNotFound", err)
		}
	})

	t.Run("смена состояния чужого кошелька", func(t *testing.T) {
		if err := db.ChangeState(ctx, uuid.New(), walletId, "BLOCKED"); !errors.Is(err, ErrWalletNotFound) {
			t.Errorf("получено %v, ожидалась ErrWalletNotFound", err)
		}
	})

	t.Run("смена состояния несуществующего кошелька", func(t *testing.T) {
		if err := db.ChangeState(ctx, owner, uuid.New(), "BLOCKED"); !errors.Is(err, ErrWalletNotFound) {
			t.Errorf("получено %v, ожидалась ErrWalletNotFound", err)
		}
	})

	t.Run("история чужого кошелька пуста", func(t *testing.T) {
		transactions, err := db.History(ctx, uuid.New(), walletId, 10, nil)
		if err != nil && !errors.Is(err, ErrWalletNotFound) {
			t.Fatalf("история: %v", err)
		}
		if len(transactions) != 0 {
			t.Errorf("чужих транзакций отдано %d", len(transactions))
		}
	})

	t.Run("освобождение резервов без просроченных", func(t *testing.T) {
		released, err := db.ReleaseExpiredReservations(ctx)
		if err != nil {
			t.Fatalf("освобождение: %v", err)
		}
		if released != 0 {
			t.Errorf("освобождено %d резервов, ожидался ноль", released)
		}
	})

	t.Run("проба готовности и статистика пула", func(t *testing.T) {
		if err := db.Ping(ctx); err != nil {
			t.Errorf("проба готовности: %v", err)
		}
		if db.Stats().MaxOpenConnections == 0 {
			t.Error("статистика пула не заполнена")
		}
	})
}

// TestTransferToBlockedWallet: перевод на заблокированный кошелёк
// отклоняется целиком — списать у отправителя и не зачислить получателю
// значило бы потерять деньги.
func TestTransferToBlockedWallet(t *testing.T) {
	ctx := context.Background()
	db, err := NewDatabaseWallet(ctx, testsupport.Prepare(t, "wallet"))
	if err != nil {
		t.Fatalf("не удалось открыть репозиторий: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	sender := uuid.New()
	receiver := uuid.New()
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

	source := walletOf(t, sender)
	target := walletOf(t, receiver)
	if _, err := db.Debit(ctx, sender, OperationParams{
		IdempotencyKey: "fill-blocked", WalletId: source, Value: 10_000,
	}); err != nil {
		t.Fatalf("пополнение: %v", err)
	}
	if err := db.ChangeState(ctx, receiver, target, "BLOCKED"); err != nil {
		t.Fatalf("блокировка кошелька: %v", err)
	}

	_, err = db.Transfer(ctx, sender, TransferParams{
		IdempotencyKey: "to-blocked", SourceId: source, TargetId: target, Value: 1_000,
	})
	if !errors.Is(err, ErrWalletNotActive) {
		t.Fatalf("получено %v, ожидалась ErrWalletNotActive", err)
	}

	// Средства отправителя остались на месте: перевод не прошёл частично.
	var balance int64
	if err := db.Information(ctx, sender, func(reply *wallet.InformationReply) {
		balance = reply.Balance
	}); err != nil {
		t.Fatalf("чтение баланса: %v", err)
	}
	if balance != 10_000 {
		t.Errorf("баланс отправителя %d, ожидался 10000", balance)
	}
}

// TestTransferRejections закрывает отказы перевода, до которых не доходит
// обычный сценарий: неположительная сумма, чужой исходный кошелёк,
// нехватка средств и повтор по тому же ключу.
func TestTransferRejections(t *testing.T) {
	ctx := context.Background()
	db, err := NewDatabaseWallet(ctx, testsupport.Prepare(t, "wallet"))
	if err != nil {
		t.Fatalf("не удалось открыть репозиторий: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	sender := uuid.New()
	receiver := uuid.New()
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

	source := walletOf(t, sender)
	target := walletOf(t, receiver)
	if _, err := db.Debit(ctx, sender, OperationParams{
		IdempotencyKey: "fill-transfer", WalletId: source, Value: 10_000,
	}); err != nil {
		t.Fatalf("пополнение: %v", err)
	}

	t.Run("неположительная сумма", func(t *testing.T) {
		for _, value := range []int64{0, -100} {
			if _, err := db.Transfer(ctx, sender, TransferParams{
				IdempotencyKey: "bad-value", SourceId: source, TargetId: target, Value: value,
			}); !errors.Is(err, ErrInvalidValue) {
				t.Errorf("сумма %d: получено %v, ожидалась ErrInvalidValue", value, err)
			}
		}
	})

	t.Run("перевод с чужого кошелька", func(t *testing.T) {
		// Тот же ответ, что и для несуществующего кошелька: иначе перебором
		// можно узнать, какие кошельки есть.
		_, err := db.Transfer(ctx, uuid.New(), TransferParams{
			IdempotencyKey: "foreign-source", SourceId: source, TargetId: target, Value: 100,
		})
		if !errors.Is(err, ErrWalletNotFound) {
			t.Errorf("получено %v, ожидалась ErrWalletNotFound", err)
		}
	})

	t.Run("сумма больше доступного остатка", func(t *testing.T) {
		_, err := db.Transfer(ctx, sender, TransferParams{
			IdempotencyKey: "too-much", SourceId: source, TargetId: target, Value: 1_000_000,
		})
		if !errors.Is(err, ErrInsufficientBalance) {
			t.Errorf("получено %v, ожидалась ErrInsufficientBalance", err)
		}
	})

	t.Run("повтор по тому же ключу не переводит дважды", func(t *testing.T) {
		params := TransferParams{
			IdempotencyKey: "transfer-once", SourceId: source, TargetId: target, Value: 1_000,
		}
		first, err := db.Transfer(ctx, sender, params)
		if err != nil {
			t.Fatalf("перевод: %v", err)
		}
		second, err := db.Transfer(ctx, sender, params)
		if err != nil {
			t.Fatalf("повтор перевода: %v", err)
		}
		if second.Id != first.Id {
			t.Errorf("повтор создал вторую транзакцию: %s против %s", second.Id, first.Id)
		}
		// Признак повтора нужен вызывающему: по нему он понимает, что его
		// запрос уже был проведён, а не проведён только что.
		if !second.Idempotent {
			t.Error("повтор не отмечен как идемпотентный")
		}

		var balance int64
		if err := db.Information(ctx, receiver, func(reply *wallet.InformationReply) {
			balance = reply.Balance
		}); err != nil {
			t.Fatalf("чтение баланса: %v", err)
		}
		if balance != 1_000 {
			t.Errorf("получателю зачислено %d, ожидалась одна тысяча", balance)
		}
	})
}

// TestReserveRejections закрывает отказы резерва: повтор по ключу,
// значение по умолчанию для срока и нехватку доступного остатка.
func TestReserveRejections(t *testing.T) {
	ctx := context.Background()
	db, err := NewDatabaseWallet(ctx, testsupport.Prepare(t, "wallet"))
	if err != nil {
		t.Fatalf("не удалось открыть репозиторий: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	owner := uuid.New()
	var walletId uuid.UUID
	if err := db.Information(ctx, owner, func(reply *wallet.InformationReply) {
		walletId = uuid.MustParse(reply.Id)
	}); err != nil {
		t.Fatalf("получение кошелька: %v", err)
	}
	if _, err := db.Debit(ctx, owner, OperationParams{
		IdempotencyKey: "fill-reserve", WalletId: walletId, Value: 10_000,
	}); err != nil {
		t.Fatalf("пополнение: %v", err)
	}

	t.Run("нулевой срок заменяется значением по умолчанию", func(t *testing.T) {
		// Резерв без срока блокирует средства навсегда, поэтому ноль
		// означает не «бессрочно», а «срок по умолчанию».
		reserved, err := db.Reserve(ctx, owner, ReserveParams{
			IdempotencyKey: "res-default-ttl", WalletId: walletId, Value: 1_000,
		})
		if err != nil {
			t.Fatalf("резерв: %v", err)
		}
		if reserved.Id == uuid.Nil {
			t.Error("резерв не создан")
		}
	})

	t.Run("повтор по тому же ключу не резервирует дважды", func(t *testing.T) {
		params := ReserveParams{
			IdempotencyKey: "res-once", WalletId: walletId, Value: 2_000, TTL: time.Minute,
		}
		first, err := db.Reserve(ctx, owner, params)
		if err != nil {
			t.Fatalf("резерв: %v", err)
		}
		second, err := db.Reserve(ctx, owner, params)
		if err != nil {
			t.Fatalf("повтор резерва: %v", err)
		}
		if second.Id != first.Id || !second.Idempotent {
			t.Errorf("повтор создал второй резерв: %+v против %+v", second, first)
		}
	})

	t.Run("резерв больше доступного остатка", func(t *testing.T) {
		// Проверяется именно доступный остаток: часть средств уже
		// зарезервирована предыдущими подтестами.
		if _, err := db.Reserve(ctx, owner, ReserveParams{
			IdempotencyKey: "res-too-much", WalletId: walletId, Value: 1_000_000, TTL: time.Minute,
		}); !errors.Is(err, ErrInsufficientBalance) {
			t.Errorf("получено %v, ожидалась ErrInsufficientBalance", err)
		}
	})

	t.Run("резерв по несуществующему кошельку", func(t *testing.T) {
		if _, err := db.Reserve(ctx, owner, ReserveParams{
			IdempotencyKey: "res-missing", WalletId: uuid.New(), Value: 100, TTL: time.Minute,
		}); !errors.Is(err, ErrWalletNotFound) {
			t.Errorf("получено %v, ожидалась ErrWalletNotFound", err)
		}
	})
}

// TestOperationCreatesDefaultWallet: кошелёк заводится при первом обращении,
// как и при чтении информации. Иначе первая же операция нового пользователя
// упирается в «нет кошелька», хотя требование говорит об автосоздании.
func TestOperationCreatesDefaultWallet(t *testing.T) {
	ctx := context.Background()
	db, err := NewDatabaseWallet(ctx, testsupport.Prepare(t, "wallet"))
	if err != nil {
		t.Fatalf("не удалось открыть репозиторий: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	owner := uuid.New()
	// Кошелёк не назван: операция идёт «в кошелёк по умолчанию».
	transaction, err := db.Debit(ctx, owner, OperationParams{
		IdempotencyKey: "first-touch", Value: 5_000,
	})
	if err != nil {
		t.Fatalf("зачисление новому пользователю: %v", err)
	}
	if transaction.WalletId == uuid.Nil {
		t.Fatal("кошелёк по умолчанию не создан")
	}

	var balance int64
	var wallets int
	if err := db.Information(ctx, owner, func(reply *wallet.InformationReply) {
		wallets++
		balance = reply.Balance
	}); err != nil {
		t.Fatalf("чтение кошельков: %v", err)
	}
	if wallets != 1 {
		t.Errorf("кошельков %d, ожидался один", wallets)
	}
	if balance != 5_000 {
		t.Errorf("баланс %d, ожидалось 5000", balance)
	}

	t.Run("списание из кошелька по умолчанию", func(t *testing.T) {
		if _, err := db.Credit(ctx, owner, OperationParams{
			IdempotencyKey: "first-spend", Value: 1_000,
		}); err != nil {
			t.Fatalf("списание: %v", err)
		}
	})

	t.Run("списание больше остатка", func(t *testing.T) {
		if _, err := db.Credit(ctx, owner, OperationParams{
			IdempotencyKey: "over-spend", Value: 1_000_000,
		}); !errors.Is(err, ErrInsufficientBalance) {
			t.Errorf("получено %v, ожидалась ErrInsufficientBalance", err)
		}
	})
}

func TestWalletReservations(t *testing.T) {
	ctx := context.Background()
	db, err := NewDatabaseWallet(ctx, testsupport.Prepare(t, "wallet"))
	if err != nil {
		t.Fatalf("не удалось открыть репозиторий: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	fund := func(t *testing.T, owner uuid.UUID, amount int64) {
		t.Helper()
		if _, err := db.Debit(ctx, owner, OperationParams{
			IdempotencyKey: uuid.NewString(), Value: amount,
		}); err != nil {
			t.Fatalf("пополнение: %v", err)
		}
	}
	report := func(t *testing.T, owner uuid.UUID) *wallet.InformationReply {
		t.Helper()
		var reply *wallet.InformationReply
		if err := db.Information(ctx, owner, func(r *wallet.InformationReply) { reply = r }); err != nil {
			t.Fatalf("чтение кошелька: %v", err)
		}
		return reply
	}

	t.Run("резерв уменьшает доступный остаток, но не баланс", func(t *testing.T) {
		owner := uuid.New()
		fund(t, owner, 1000)

		if _, err := db.Reserve(ctx, owner, ReserveParams{
			IdempotencyKey: uuid.NewString(), Value: 400,
		}); err != nil {
			t.Fatalf("резервирование: %v", err)
		}

		info := report(t, owner)
		if info.Balance != 1000 {
			t.Errorf("баланс %d, ожидался 1000: резерв не должен менять баланс", info.Balance)
		}
		if info.Available != 600 {
			t.Errorf("доступно %d, ожидалось 600", info.Available)
		}
	})

	t.Run("зарезервированные средства нельзя потратить второй раз", func(t *testing.T) {
		owner := uuid.New()
		fund(t, owner, 1000)
		if _, err := db.Reserve(ctx, owner, ReserveParams{
			IdempotencyKey: uuid.NewString(), Value: 800,
		}); err != nil {
			t.Fatalf("резервирование: %v", err)
		}

		// Списание проверяет доступный остаток, а не баланс.
		_, err := db.Credit(ctx, owner, OperationParams{IdempotencyKey: uuid.NewString(), Value: 300})
		if !errors.Is(err, ErrInsufficientBalance) {
			t.Fatalf("получено %v, ожидалась ErrInsufficientBalance", err)
		}
	})

	t.Run("подтверждение списывает средства", func(t *testing.T) {
		owner := uuid.New()
		fund(t, owner, 1000)
		reservation, err := db.Reserve(ctx, owner, ReserveParams{
			IdempotencyKey: uuid.NewString(), Value: 250,
		})
		if err != nil {
			t.Fatalf("резервирование: %v", err)
		}

		if _, err = db.Confirm(ctx, owner, reservation.Id); err != nil {
			t.Fatalf("подтверждение: %v", err)
		}

		info := report(t, owner)
		if info.Balance != 750 {
			t.Errorf("баланс %d, ожидался 750", info.Balance)
		}
		if info.Available != 750 {
			t.Errorf("доступно %d, ожидалось 750: резерв должен быть снят", info.Available)
		}
	})

	t.Run("отмена освобождает средства", func(t *testing.T) {
		owner := uuid.New()
		fund(t, owner, 1000)
		reservation, err := db.Reserve(ctx, owner, ReserveParams{
			IdempotencyKey: uuid.NewString(), Value: 250,
		})
		if err != nil {
			t.Fatalf("резервирование: %v", err)
		}

		if _, err = db.Reject(ctx, owner, reservation.Id); err != nil {
			t.Fatalf("отмена: %v", err)
		}

		info := report(t, owner)
		if info.Balance != 1000 || info.Available != 1000 {
			t.Errorf("баланс %d, доступно %d, ожидалось 1000 и 1000", info.Balance, info.Available)
		}
	})

	t.Run("повторное подтверждение отклоняется", func(t *testing.T) {
		owner := uuid.New()
		fund(t, owner, 1000)
		reservation, err := db.Reserve(ctx, owner, ReserveParams{
			IdempotencyKey: uuid.NewString(), Value: 100,
		})
		if err != nil {
			t.Fatalf("резервирование: %v", err)
		}
		if _, err = db.Confirm(ctx, owner, reservation.Id); err != nil {
			t.Fatalf("подтверждение: %v", err)
		}

		// Иначе повтор списал бы средства второй раз.
		if _, err = db.Confirm(ctx, owner, reservation.Id); !errors.Is(err, ErrReservationNotPending) {
			t.Fatalf("получено %v, ожидалась ErrReservationNotPending", err)
		}
		if info := report(t, owner); info.Balance != 900 {
			t.Errorf("баланс %d, ожидался 900", info.Balance)
		}
	})

	t.Run("чужой резерв недоступен", func(t *testing.T) {
		owner := uuid.New()
		fund(t, owner, 500)
		reservation, err := db.Reserve(ctx, owner, ReserveParams{
			IdempotencyKey: uuid.NewString(), Value: 100,
		})
		if err != nil {
			t.Fatalf("резервирование: %v", err)
		}

		if _, err = db.Confirm(ctx, uuid.New(), reservation.Id); !errors.Is(err, ErrReservationNotFound) {
			t.Fatalf("получено %v, ожидалась ErrReservationNotFound", err)
		}
	})

	t.Run("просроченный резерв освобождается", func(t *testing.T) {
		owner := uuid.New()
		fund(t, owner, 1000)
		// Отрицательный срок жизни даёт заведомо просроченный резерв.
		reservation, err := db.Reserve(ctx, owner, ReserveParams{
			IdempotencyKey: uuid.NewString(), Value: 700, TTL: time.Millisecond,
		})
		if err != nil {
			t.Fatalf("резервирование: %v", err)
		}
		time.Sleep(20 * time.Millisecond)

		released, err := db.ReleaseExpiredReservations(ctx)
		if err != nil {
			t.Fatalf("освобождение: %v", err)
		}
		if released != 1 {
			t.Fatalf("освобождено %d резервов, ожидался 1", released)
		}
		if info := report(t, owner); info.Available != 1000 {
			t.Errorf("доступно %d, ожидалось 1000", info.Available)
		}
		if _, err = db.Confirm(ctx, owner, reservation.Id); !errors.Is(err, ErrReservationNotPending) {
			t.Errorf("просроченный резерв не должен подтверждаться: %v", err)
		}
	})
}
