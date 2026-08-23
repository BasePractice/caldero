package main

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	"wish/services"
	"wish/services/shared/account"

	"github.com/google/uuid"

	_ "github.com/lib/pq"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Database interface {
	Create(ctx context.Context, account account.CreateAccount, operator *services.AuthorizedUser) (uuid.UUID, error)
	Get(ctx context.Context, id uuid.UUID) (*account.Account, error)
	// Close освобождает соединения с БД
	Close() error
}

type ds struct {
	db *sql.DB
}

func (d ds) Create(ctx context.Context, a account.CreateAccount, operator *services.AuthorizedUser) (uuid.UUID, error) {
	var id uuid.UUID
	err := d.db.QueryRowContext(ctx, `
		INSERT INTO account (user_id, type, credit_id)
		VALUES ($1, $2, $3) RETURNING id`,
		operator.Id, a.Type, a.CreditId).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("creating %s account for user %s: %w", a.Type, operator.Id, err)
	}
	return id, nil
}

func (d ds) Get(ctx context.Context, id uuid.UUID) (*account.Account, error) {
	var a account.Account
	var startedAt sql.NullTime
	err := d.db.QueryRowContext(ctx, `
		SELECT id, user_id, type, credit_id, state, balance, started_at, created_at, updated_at
		FROM account WHERE id = $1`, id).
		Scan(&a.Id, &a.UserId, &a.Type, &a.CreditId, &a.State, &a.Balance,
			&startedAt, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("loading account %s: %w", id, err)
	}
	if startedAt.Valid {
		a.StartedAt = &startedAt.Time
	}
	return &a, nil
}

func (d ds) Close() error {
	return d.db.Close()
}

func NewDatabase(cfg services.Config) (Database, error) {
	db, err := services.NewDatabase(cfg, migrations)
	if err != nil {
		return nil, fmt.Errorf("opening account database: %w", err)
	}
	return &ds{db}, nil
}
