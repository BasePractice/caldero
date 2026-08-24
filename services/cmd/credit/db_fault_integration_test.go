//go:build integration

package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"wish/services"
	"wish/services/shared/credit"
	"wish/services/testsupport"

	"github.com/google/uuid"
)

// TestRecordPaymentAtomicUnderFailure проверяет не покрытие охранных ветвей,
// а свойство, ради которого они написаны: сбой на любом шаге транзакции
// не оставляет платёж записанным без изменения кредита — и наоборот.
//
// Половина платежа хуже, чем ни одного: платёж без учёта в already_paid
// означает, что заёмщик заплатил, а долг не уменьшился.
func TestRecordPaymentAtomicUnderFailure(t *testing.T) {
	ctx := context.Background()
	cfg := testsupport.Prepare(t, "credit_fault")

	// Обычный конструктор нужен, чтобы применились миграции: репозитория
	// со сбоями не существует до того, как появилась схема.
	db, err := NewDatabase(ctx, cfg)
	if err != nil {
		t.Fatalf("не удалось открыть репозиторий: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	faultyDB, fault := testsupport.OpenFaulty(t, cfg)
	faulty := ds{db: faultyDB}

	operator := &services.AuthorizedUser{Id: uuid.New()}
	id, err := db.Create(ctx, credit.CreateCredit{
		UserId: uuid.New(), Type: "SIMPLE", Kind: "ANN",
		Month: 12, Rate: 20 * credit.BasisPointsInPercent, Balance: 600_000,
	}, operator)
	if err != nil {
		t.Fatalf("создание кредита: %v", err)
	}

	// Состояние берётся исправным подключением: проверять данные тем же
	// соединением, в которое внедряются сбои, — значит проверять обёртку.
	state := func(t *testing.T) (credit.Amount, int) {
		t.Helper()
		var (
			paid     credit.Amount
			payments int
		)
		if err := rawDB(t, db).QueryRowContext(ctx,
			`SELECT c.already_paid, (SELECT count(*) FROM payment WHERE credit_id = c.id)
			 FROM credit c WHERE c.id = $1`, id).Scan(&paid, &payments); err != nil {
			t.Fatalf("чтение состояния: %v", err)
		}
		return paid, payments
	}

	const amount = credit.Amount(50_000)
	before, paymentsBefore := state(t)

	// Номер сбойного запроса растёт, пока операция не пройдёт целиком:
	// так проверяется каждый шаг транзакции, а не выбранный наугад.
	steps := 0
	for n := 1; ; n++ {
		fault.FailAt(n)
		err := faulty.RecordPayment(ctx, PaymentRecord{
			CreditId:       id,
			IdempotencyKey: fmt.Sprintf("fault-%d", n),
			NeedValue:      amount,
			Amount:         amount,
		})
		fired := fault.Fired()
		fault.Disarm()

		if !fired {
			// Запросов в транзакции меньше, чем n: все шаги проверены,
			// а этот проход довёл платёж до конца.
			if err != nil {
				t.Fatalf("платёж без внедрённого сбоя не прошёл: %v", err)
			}
			break
		}
		steps++

		if !errors.Is(err, testsupport.ErrFault) {
			t.Fatalf("сбой на %d-м запросе не дошёл до вызывающего кода: %v", n, err)
		}
		if paid, payments := state(t); paid != before || payments != paymentsBefore {
			t.Fatalf("сбой на %d-м запросе оставил след: already_paid %d вместо %d, платежей %d вместо %d",
				n, paid, before, payments, paymentsBefore)
		}
	}

	if steps < 3 {
		t.Fatalf("проверено шагов транзакции: %d, ожидалось не меньше трёх", steps)
	}
	if paid, payments := state(t); paid != before+amount || payments != paymentsBefore+1 {
		t.Fatalf("прошедший платёж не учтён: already_paid %d, платежей %d", paid, payments)
	}
}
