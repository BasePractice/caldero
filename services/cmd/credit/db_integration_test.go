//go:build integration

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"wish/services"
	"wish/services/shared/credit"
	"wish/services/testsupport"

	"github.com/google/uuid"
)

func TestCreditRepository(t *testing.T) {
	ctx := context.Background()
	db, err := NewDatabase(ctx, testsupport.Prepare(t, "credit"))
	if err != nil {
		t.Fatalf("не удалось открыть репозиторий: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	operator := &services.AuthorizedUser{Id: uuid.New()}
	borrower := uuid.New()

	t.Run("создание и чтение возвращают все поля расчёта", func(t *testing.T) {
		created := credit.CreateCredit{
			UserId: borrower, Type: "SIMPLE", Kind: "ANN",
			Month: 36, Rate: 24 * credit.BasisPointsInPercent, Balance: 1_200_000,
		}
		id, err := db.Create(ctx, created, operator)
		if err != nil {
			t.Fatalf("создание: %v", err)
		}

		loaded, err := db.Get(ctx, id)
		if err != nil {
			t.Fatalf("чтение: %v", err)
		}
		if loaded.UserId != borrower {
			t.Errorf("user_id = %s, ожидался %s", loaded.UserId, borrower)
		}
		if loaded.CreatorId != operator.Id {
			t.Errorf("creator_id = %s, ожидался %s", loaded.CreatorId, operator.Id)
		}
		if loaded.Month != 36 || loaded.Rate != 24*credit.BasisPointsInPercent || loaded.Balance != 1_200_000 {
			t.Errorf("не совпали параметры кредита: %+v", loaded)
		}
		// Именно эти три поля запрос когда-то не выбирал, из-за чего частично
		// погашенный кредит считался как только что выданный.
		if loaded.AlreadyPaid != 0 {
			t.Errorf("already_paid = %d, ожидался 0", loaded.AlreadyPaid)
		}
		if loaded.CreatedAt.IsZero() {
			t.Error("created_at не заполнен")
		}
		if loaded.LastPaidAt != nil {
			t.Errorf("last_paid_at = %v, ожидался nil", loaded.LastPaidAt)
		}
	})

	t.Run("отсутствующий кредит отличается от сбоя базы", func(t *testing.T) {
		if _, err := db.Get(ctx, uuid.New()); !errors.Is(err, ErrCreditNotFound) {
			t.Fatalf("ожидалась ErrCreditNotFound, получено %v", err)
		}
	})

	t.Run("частично погашенный кредит читается с датой платежа", func(t *testing.T) {
		id, err := db.Create(ctx, credit.CreateCredit{
			UserId: uuid.New(), Type: "MICRO", Kind: "ANN",
			Month: 12, Rate: 30 * credit.BasisPointsInPercent, Balance: 500_000,
		}, operator)
		if err != nil {
			t.Fatalf("создание: %v", err)
		}

		paidAt := time.Now().Add(-24 * time.Hour).UTC()
		if _, err = rawDB(t, db).ExecContext(ctx,
			"UPDATE credit SET already_paid = $1, last_paid_at = $2 WHERE id = $3",
			50_000, paidAt, id); err != nil {
			t.Fatalf("обновление: %v", err)
		}

		loaded, err := db.Get(ctx, id)
		if err != nil {
			t.Fatalf("чтение: %v", err)
		}
		if loaded.AlreadyPaid != 50_000 {
			t.Errorf("already_paid = %d, ожидался 50000", loaded.AlreadyPaid)
		}
		if loaded.LastPaidAt == nil {
			t.Fatal("last_paid_at не прочитан")
		}
		if diff := loaded.LastPaidAt.Sub(paidAt); diff > time.Second || diff < -time.Second {
			t.Errorf("last_paid_at = %s, ожидался %s", loaded.LastPaidAt, paidAt)
		}
	})

	t.Run("график считается по прочитанным данным", func(t *testing.T) {
		id, err := db.Create(ctx, credit.CreateCredit{
			UserId: uuid.New(), Type: "IPOT", Kind: "ANN",
			Month: 60, Rate: 10 * credit.BasisPointsInPercent, Balance: 1_000_000,
		}, operator)
		if err != nil {
			t.Fatalf("создание: %v", err)
		}
		loaded, err := db.Get(ctx, id)
		if err != nil {
			t.Fatalf("чтение: %v", err)
		}
		payments, err := monthPaymentCalculation(*loaded)
		if err != nil {
			t.Fatalf("расчёт: %v", err)
		}
		if len(payments) != 60 {
			t.Fatalf("платежей %d, ожидалось 60", len(payments))
		}
		if payments[0].Value != 21247 {
			t.Errorf("платёж %d, ожидался 21247", payments[0].Value)
		}
	})

	t.Run("второй кредит того же типа разрешён", func(t *testing.T) {
		user := uuid.New()
		same := credit.CreateCredit{UserId: user, Type: "SIMPLE", Kind: "ANN", Month: 12, Rate: 15 * credit.BasisPointsInPercent, Balance: 100_000}
		if _, err := db.Create(ctx, same, operator); err != nil {
			t.Fatalf("первый кредит: %v", err)
		}
		// Прежнее ограничение UNIQUE (user_id, type) запрещало это.
		if _, err := db.Create(ctx, same, operator); err != nil {
			t.Fatalf("второй кредит того же типа: %v", err)
		}
	})

	t.Run("отрицательная сумма отвергается схемой", func(t *testing.T) {
		_, err := rawDB(t, db).ExecContext(ctx,
			"INSERT INTO credit (user_id, creator_id, balance) VALUES ($1, $2, $3)",
			uuid.New(), operator.Id, -100)
		if err == nil {
			t.Fatal("отрицательный баланс кредита должен отвергаться")
		}
	})
}
