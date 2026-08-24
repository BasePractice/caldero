//go:build integration

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	wallet "wish/middleware/wallet/v1"
	"wish/services/testsupport"

	"github.com/google/uuid"
)

// maxTransactionSteps ограничивает перебор: операция, которая на полусотне
// запросов так и не дошла до конца, — это зацикливание, а не длинная
// транзакция, и тест обязан сказать об этом, а не висеть.
const maxTransactionSteps = 50

// TestOperationsAtomicUnderFailure проверяет свойство, ради которого операции
// кошелька собраны в транзакции: сбой базы на любом её шаге не оставляет
// частично применённых изменений.
//
// Для кошелька это прямой вопрос денег: списание без встречного зачисления
// или транзакция без изменения баланса — это расхождение, которое потом
// нечем объяснить.
func TestOperationsAtomicUnderFailure(t *testing.T) {
	ctx := context.Background()
	cfg := testsupport.Prepare(t, "wallet_fault")

	// Обычный конструктор нужен, чтобы применились миграции, и служит
	// исправным подключением: проверять состояние тем же соединением,
	// в которое внедряются сбои, — значит проверять обёртку.
	db, err := NewDatabaseWallet(ctx, cfg)
	if err != nil {
		t.Fatalf("не удалось открыть репозиторий: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	faultyDB, fault := testsupport.OpenFaulty(t, cfg)
	faulty := ds{db: faultyDB}

	owner, recipient := uuid.New(), uuid.New()
	source := firstWallet(ctx, t, db, owner)
	target := firstWallet(ctx, t, db, recipient)

	if _, err = db.Debit(ctx, owner, OperationParams{
		IdempotencyKey: "начальное пополнение", WalletId: source, Value: 100_000,
	}); err != nil {
		t.Fatalf("пополнение кошелька: %v", err)
	}

	// В снимок входит всё, что операция могла бы изменить наполовину:
	// балансы обоих кошельков, число транзакций и сумма в резерве.
	snapshot := func(t *testing.T) string {
		t.Helper()
		var state strings.Builder
		for _, id := range []uuid.UUID{source, target} {
			var (
				walletState string
				balance     int64
				count       int64
				reserved    int64
			)
			if err := rawDB(t, db).QueryRowContext(ctx, `
				SELECT w.state, w.balance,
				       (SELECT count(*) FROM transaction t
				         WHERE t.target = w.id OR t.source = w.id),
				       (SELECT coalesce(sum(t.value), 0) FROM transaction t
				         WHERE t.target = w.id AND t.state = 'RESERVED')
				FROM wallet w WHERE w.id = $1`, id).
				Scan(&walletState, &balance, &count, &reserved); err != nil {
				t.Fatalf("чтение состояния кошелька %s: %v", id, err)
			}
			fmt.Fprintf(&state, "%s=%s/%d/%d/%d ", id, walletState, balance, count, reserved)
		}
		return state.String()
	}

	// prepare получает номер попытки, готовит для неё исходные данные
	// и возвращает саму операцию: снимок состояния снимается уже после
	// подготовки, иначе её следы выглядели бы как след сбоя.
	sweep := func(t *testing.T, prepare func(attempt int) func() error) {
		t.Helper()
		steps := 0
		for n := 1; n <= maxTransactionSteps; n++ {
			operation := prepare(n)
			before := snapshot(t)

			fault.FailAt(n)
			err := operation()
			fired := fault.Fired()
			// Снятие обязательно: операция могла закончиться раньше,
			// чем дошла до n-го запроса, и взведённый сбой достался бы
			// проверке состояния.
			fault.Disarm()

			if !fired {
				// Запросов в операции меньше, чем n: все её шаги уже
				// проверены, а этот проход прошёл целиком.
				if err != nil {
					t.Fatalf("операция без внедрённого сбоя не прошла: %v", err)
				}
				if steps < 2 {
					t.Fatalf("проверено шагов транзакции: %d, ожидалось не меньше двух", steps)
				}
				return
			}
			steps++

			if !errors.Is(err, testsupport.ErrFault) {
				t.Fatalf("сбой на %d-м запросе не дошёл до вызывающего кода: %v", n, err)
			}
			if after := snapshot(t); after != before {
				t.Fatalf("сбой на %d-м запросе изменил состояние:\nбыло  %s\nстало %s", n, before, after)
			}
		}
		t.Fatalf("операция не завершилась за %d запросов", maxTransactionSteps)
	}

	t.Run("перевод", func(t *testing.T) {
		sweep(t, func(attempt int) func() error {
			return func() error {
				_, err := faulty.Transfer(ctx, owner, TransferParams{
					IdempotencyKey: fmt.Sprintf("перевод-%d", attempt),
					SourceId:       source, TargetId: target, Value: 1_000,
				})
				return err
			}
		})
	})

	t.Run("списание", func(t *testing.T) {
		sweep(t, func(attempt int) func() error {
			return func() error {
				_, err := faulty.Credit(ctx, owner, OperationParams{
					IdempotencyKey: fmt.Sprintf("списание-%d", attempt),
					WalletId:       source, Value: 1_000,
				})
				return err
			}
		})
	})

	t.Run("резерв", func(t *testing.T) {
		sweep(t, func(attempt int) func() error {
			return func() error {
				_, err := faulty.Reserve(ctx, owner, ReserveParams{
					IdempotencyKey: fmt.Sprintf("резерв-%d", attempt),
					WalletId:       source, Value: 1_000, TTL: time.Minute,
				})
				return err
			}
		})
	})

	t.Run("подтверждение резерва", func(t *testing.T) {
		sweep(t, func(attempt int) func() error {
			// Резерв под каждую попытку создаётся заранее исправным
			// подключением: подтверждать нечего, пока его нет.
			reservation, err := db.Reserve(ctx, owner, ReserveParams{
				IdempotencyKey: fmt.Sprintf("резерв-под-подтверждение-%d", attempt),
				WalletId:       source, Value: 1_000, TTL: time.Minute,
			})
			if err != nil {
				t.Fatalf("создание резерва: %v", err)
			}
			return func() error {
				_, err := faulty.Confirm(ctx, owner, reservation.Id)
				return err
			}
		})
	})

	t.Run("отмена резерва", func(t *testing.T) {
		sweep(t, func(attempt int) func() error {
			reservation, err := db.Reserve(ctx, owner, ReserveParams{
				IdempotencyKey: fmt.Sprintf("резерв-под-отмену-%d", attempt),
				WalletId:       source, Value: 1_000, TTL: time.Minute,
			})
			if err != nil {
				t.Fatalf("создание резерва: %v", err)
			}
			return func() error {
				_, err := faulty.Reject(ctx, owner, reservation.Id)
				return err
			}
		})
	})
}

// firstWallet возвращает кошелёк пользователя, создавая его при первом
// обращении, — как это делает сам сервис.
func firstWallet(ctx context.Context, t *testing.T, db DatabaseWallet, user uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := db.Information(ctx, user, func(reply *wallet.InformationReply) {
		if id == uuid.Nil {
			parsed, err := uuid.Parse(reply.Id)
			if err != nil {
				t.Fatalf("неразбираемый идентификатор кошелька %q: %v", reply.Id, err)
			}
			id = parsed
		}
	}); err != nil {
		t.Fatalf("чтение кошельков пользователя %s: %v", user, err)
	}
	if id == uuid.Nil {
		t.Fatalf("у пользователя %s нет кошелька", user)
	}
	return id
}
