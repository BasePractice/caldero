//go:build integration

package main

import (
	"context"
	"testing"

	"wish/services"
	"wish/services/shared/account"
	"wish/services/testsupport"

	"github.com/google/uuid"
)

func TestAccountRepository(t *testing.T) {
	ctx := context.Background()
	db, err := NewDatabase(ctx, testsupport.Prepare(t, "account"))
	if err != nil {
		t.Fatalf("не удалось открыть репозиторий: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	operator := &services.AuthorizedUser{Id: uuid.New()}
	owner := uuid.New()

	t.Run("создание и чтение дебетового счёта", func(t *testing.T) {
		id, err := db.Create(ctx, account.CreateAccount{
			UserId: owner, Type: account.TypeDebit,
		}, operator)
		if err != nil {
			t.Fatalf("создание: %v", err)
		}

		loaded, err := db.Get(ctx, id)
		if err != nil {
			t.Fatalf("чтение: %v", err)
		}
		if loaded.Id != id || loaded.UserId != owner {
			t.Errorf("получено %+v", loaded)
		}
		if loaded.Type != account.TypeDebit {
			t.Errorf("тип %q, ожидался %q", loaded.Type, account.TypeDebit)
		}
		if loaded.CreditId != nil {
			t.Errorf("credit_id = %v, ожидался nil", loaded.CreditId)
		}
		// Значения по умолчанию задаёт схема, и запрос обязан их вернуть:
		// иначе новый счёт выглядит заблокированным или без даты создания.
		if loaded.State != "ACTIVE" {
			t.Errorf("состояние %q, ожидалось ACTIVE", loaded.State)
		}
		if loaded.Balance != 0 {
			t.Errorf("баланс %s, ожидался нулевой", loaded.Balance)
		}
		if loaded.StartedAt != nil {
			t.Errorf("started_at = %v, ожидался nil у незапущенного счёта", loaded.StartedAt)
		}
		if loaded.CreatedAt.IsZero() || loaded.UpdatedAt.IsZero() {
			t.Errorf("не заполнены отметки времени: %+v", loaded)
		}
	})

	t.Run("кредитный счёт хранит ссылку на кредит", func(t *testing.T) {
		creditId := uuid.New()
		id, err := db.Create(ctx, account.CreateAccount{
			UserId: owner, Type: account.TypeCredit, CreditId: &creditId,
		}, operator)
		if err != nil {
			t.Fatalf("создание: %v", err)
		}

		loaded, err := db.Get(ctx, id)
		if err != nil {
			t.Fatalf("чтение: %v", err)
		}
		if loaded.CreditId == nil || *loaded.CreditId != creditId {
			t.Errorf("credit_id = %v, ожидался %s", loaded.CreditId, creditId)
		}
	})

	// Ограничение схемы должно срабатывать даже в обход проверки модели:
	// репозиторий — последний рубеж, и молча записать такой счёт нельзя.
	t.Run("дебетовый счёт со ссылкой на кредит отвергается схемой", func(t *testing.T) {
		creditId := uuid.New()
		if _, err := db.Create(ctx, account.CreateAccount{
			UserId: owner, Type: account.TypeDebit, CreditId: &creditId,
		}, operator); err == nil {
			t.Fatal("счёт записан, ожидалось нарушение ограничения")
		}
	})

	t.Run("отсутствующий счёт не читается", func(t *testing.T) {
		if _, err := db.Get(ctx, uuid.New()); err == nil {
			t.Fatal("несуществующий счёт прочитан")
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
