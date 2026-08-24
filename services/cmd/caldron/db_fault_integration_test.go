//go:build integration

package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"wish/services/shared/caldron"
	"wish/services/testsupport"

	"github.com/google/uuid"
)

// maxTransactionSteps ограничивает перебор: операция, которая на полусотне
// запросов так и не дошла до конца, — это зацикливание, а не длинная
// транзакция, и тест обязан сказать об этом, а не висеть.
const maxTransactionSteps = 50

// TestCaldronAtomicUnderFailure проверяет свойство, ради которого операции
// котла собраны в транзакции: сбой базы на любом шаге не оставляет
// частично применённых изменений.
//
// Для котла это вопрос денег так же, как и для кошелька: участник,
// отмеченный внёсшим при несобранном котле, или наполовину заменённый
// список подарков — это расхождение, которое участникам не объяснить.
func TestCaldronAtomicUnderFailure(t *testing.T) {
	ctx := context.Background()
	cfg := testsupport.Prepare(t, "caldron_fault")

	// Обычный конструктор нужен, чтобы применились миграции, и служит
	// исправным подключением: проверять состояние тем же соединением,
	// в которое внедряются сбои, — значит проверять обёртку.
	db, err := NewDatabase(ctx, cfg)
	if err != nil {
		t.Fatalf("не удалось открыть репозиторий: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	faultyDB, fault := testsupport.OpenFaulty(t, cfg)
	faulty := ds{db: faultyDB}

	store, ok := db.(*ds)
	if !ok {
		t.Fatalf("неожиданный тип репозитория %T", db)
	}

	// Снимок берётся по всей схеме, а не по одной строке: операции котла
	// пишут в три таблицы, и частичное изменение может остаться в любой
	// из них. Подготовка к попытке успевает попасть в снимок до сбоя.
	snapshot := func(t *testing.T) string {
		t.Helper()
		var state strings.Builder
		queries := []string{
			`SELECT id, state, wallet_id, arbiter_id, settled_at, cancelled_at
			 FROM caldron ORDER BY id`,
			`SELECT caldron_id, user_id, state, expected, contributed
			 FROM participant ORDER BY caldron_id, user_id`,
			`SELECT caldron_id, user_id, product_id, price FROM gift
			 ORDER BY caldron_id, user_id, product_id`,
		}
		for _, query := range queries {
			appendRows(ctx, t, store.db, query, &state)
		}
		return state.String()
	}

	// prepare получает номер попытки, готовит для неё исходные данные
	// и возвращает саму операцию: снимок снимается уже после подготовки,
	// иначе её следы выглядели бы как след сбоя.
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
				t.Fatalf("сбой на %d-м запросе изменил состояние:\nбыло\n%sстало\n%s", n, before, after)
			}
		}
		t.Fatalf("операция не завершилась за %d запросов", maxTransactionSteps)
	}

	newCaldron := func(t *testing.T, creator uuid.UUID) caldron.Caldron {
		t.Helper()
		pot, err := db.Create(ctx, caldron.Caldron{
			CreatorId: creator, Title: "Сбой посреди транзакции", Type: caldron.TypeGift,
			CreatorParticipates: false, Mode: caldron.ModeFixed, Amount: 1_000_00,
		})
		if err != nil {
			t.Fatalf("создание котла: %v", err)
		}
		return pot
	}

	pot := newCaldron(t, uuid.New())

	t.Run("добавление участника", func(t *testing.T) {
		sweep(t, func(int) func() error {
			member := uuid.New()
			return func() error {
				_, err := faulty.AddParticipant(ctx, pot.Id, caldron.AddParticipant{UserId: member})
				return err
			}
		})
	})

	t.Run("удаление участника", func(t *testing.T) {
		sweep(t, func(int) func() error {
			member := uuid.New()
			if _, err := db.AddParticipant(ctx, pot.Id, caldron.AddParticipant{UserId: member}); err != nil {
				t.Fatalf("добавление участника: %v", err)
			}
			return func() error {
				_, err := faulty.RemoveParticipant(ctx, pot.Id, member)
				return err
			}
		})
	})

	t.Run("начало взноса", func(t *testing.T) {
		sweep(t, func(int) func() error {
			member := uuid.New()
			if _, err := db.AddParticipant(ctx, pot.Id, caldron.AddParticipant{UserId: member}); err != nil {
				t.Fatalf("добавление участника: %v", err)
			}
			return func() error {
				_, _, err := faulty.StartContribution(ctx, pot.Id, member, 1_000_00)
				return err
			}
		})
	})

	t.Run("отметка о взносе", func(t *testing.T) {
		// Отдельный котёл на каждую попытку: отмеченный взнос собирает
		// котёл целиком и переводит его в «готов», а повторить это
		// на одном и том же котле нельзя.
		sweep(t, func(int) func() error {
			own := newCaldron(t, uuid.New())
			member := uuid.New()
			if _, err := db.AddParticipant(ctx, own.Id, caldron.AddParticipant{UserId: member}); err != nil {
				t.Fatalf("добавление участника: %v", err)
			}
			return func() error {
				_, err := faulty.MarkPaid(ctx, own.Id, member, 1_000_00)
				return err
			}
		})
	})

	t.Run("смена состояния котла", func(t *testing.T) {
		sweep(t, func(int) func() error {
			own := newCaldron(t, uuid.New())
			return func() error {
				_, err := faulty.Transition(ctx, own.Id, caldron.StateCancelled, caldron.ActorCreator)
				return err
			}
		})
	})

	t.Run("назначение арбитра", func(t *testing.T) {
		sweep(t, func(int) func() error {
			arbiter := uuid.New()
			own := newCaldron(t, uuid.New())
			return func() error {
				_, err := faulty.SetArbiter(ctx, own.Id, &arbiter)
				return err
			}
		})
	})

	t.Run("замена списка подарков", func(t *testing.T) {
		// Список заменяется целиком: удаление старых подарков без записи
		// новых оставило бы участника без выбора.
		sweep(t, func(attempt int) func() error {
			member := uuid.New()
			gifts := []caldron.Gift{
				newGift(pot.Id, member, fmt.Sprintf("подарок-%d-a", attempt)),
				newGift(pot.Id, member, fmt.Sprintf("подарок-%d-b", attempt)),
			}
			return func() error {
				_, err := faulty.ReplaceGifts(ctx, pot.Id, member, gifts)
				return err
			}
		})
	})

	// Сбои кончились: репозиторий обязан остаться рабочим, а не унести
	// с собой соединение.
	if _, err := db.Caldron(ctx, pot.Id); err != nil {
		t.Fatalf("котёл не читается после серии сбоев: %v", err)
	}
	var dangling int
	if err := store.db.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_stat_activity
		 WHERE datname = current_database() AND state = 'idle in transaction'`).
		Scan(&dangling); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("чтение состояния соединений: %v", err)
	}
	if dangling != 0 {
		t.Errorf("после сбоев осталось незакрытых транзакций: %d", dangling)
	}
}

// appendRows дописывает в снимок все строки запроса. Колонки читаются
// как any: снимок сравнивается целиком, и разбирать типы незачем.
func appendRows(ctx context.Context, t *testing.T, db *sql.DB, query string, state *strings.Builder) {
	t.Helper()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("чтение состояния: %v", err)
	}
	defer func() {
		// Настоящая причина сбоя придёт из rows.Err().
		_ = rows.Close()
	}()

	columns, err := rows.Columns()
	if err != nil {
		t.Fatalf("чтение состояния: %v", err)
	}
	values := make([]any, len(columns))
	pointers := make([]any, len(columns))
	for i := range values {
		pointers[i] = &values[i]
	}
	for rows.Next() {
		if err = rows.Scan(pointers...); err != nil {
			t.Fatalf("чтение состояния: %v", err)
		}
		fmt.Fprintf(state, "%v\n", values)
	}
	if err = rows.Err(); err != nil {
		t.Fatalf("чтение состояния: %v", err)
	}
}

func newGift(pot, user uuid.UUID, product string) caldron.Gift {
	return caldron.Gift{
		CaldronId: pot, UserId: user, Provider: "OZON", ProductId: product,
		Title: "Подарок", Price: 1_000_00, PriceAt: time.Now().UTC().Truncate(time.Second),
	}
}
