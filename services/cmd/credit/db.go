package main

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"

	"wish/services"
	"wish/services/shared/credit"

	_ "github.com/lib/pq"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Database interface {
	Create(ctx context.Context, credit credit.CreateCredit, operator *services.AuthorizedUser) (int64, error)
	Get(ctx context.Context, id uint64) (*credit.Credit, error)
	// Close освобождает соединения с БД
	Close() error
}

type ds struct {
	db *sql.DB
}

// ErrCreditNotFound отделяет отсутствие записи от прочих ошибок БД:
// раньше обработчик отвечал 404 на любую ошибку, включая недоступную базу.
var ErrCreditNotFound = errors.New("credit not found")

func (d ds) Get(ctx context.Context, id uint64) (*credit.Credit, error) {
	var c credit.Credit
	var lastPaidAt sql.NullTime

	// already_payed, created_at и last_payed_at управляют расчётом графика:
	// без них частично погашенный кредит считался как только что выданный.
	err := d.db.QueryRowContext(ctx, `SELECT
		user_id, creator_id, type, percent, balance, kind, month,
		already_payed, created_at, last_payed_at
		FROM credit WHERE id = $1`, id).
		Scan(&c.UserId, &c.CreatorId, &c.Type, &c.Percent, &c.Balance, &c.Kind, &c.Month,
			&c.AlreadyPaid, &c.CreatedAt, &lastPaidAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("credit %d: %w", id, ErrCreditNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("loading credit %d: %w", id, err)
	}
	if lastPaidAt.Valid {
		c.LastPaidAt = &lastPaidAt.Time
	}
	return &c, nil
}

func (d ds) Create(ctx context.Context, c credit.CreateCredit, operator *services.AuthorizedUser) (int64, error) {
	var id int64
	if err := d.db.QueryRowContext(ctx, `
		INSERT INTO credit (user_id, creator_id, type, percent, balance, kind, month) 
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		c.UserId, operator.Id, c.Type, c.Percent, c.Balance, c.Kind, c.Month).Scan(&id); err != nil {
		return 0, fmt.Errorf("failed to create credit: %w", err)
	}
	return id, nil
}

func (d ds) Close() error {
	return d.db.Close()
}

func NewDatabase(cfg services.Config) (Database, error) {
	db, err := services.NewDatabase(cfg, migrations)
	if err != nil {
		return nil, fmt.Errorf("opening credit database: %w", err)
	}
	return &ds{db}, nil
}
