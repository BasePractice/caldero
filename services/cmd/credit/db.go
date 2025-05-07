package main

import (
	"context"
	"database/sql"
	"embed"
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
}

type ds struct {
	db *sql.DB
}

func (d ds) Get(ctx context.Context, id uint64) (*credit.Credit, error) {
	var c credit.Credit

	err := d.db.QueryRowContext(ctx, `SELECT 
    user_id, creator_id, type, percent, balance, kind, month FROM credit WHERE id = $1`,
		id).Scan(&c.UserId, &c.CreatorId, &c.Type, &c.Percent, &c.Balance, &c.Kind, &c.Month)
	if err != nil {
		return nil, err
	}
	return &c, err
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

func NewDatabase() Database {
	db, err := services.NewDatabase(migrations)
	if err != nil {
		return nil
	}
	return &ds{db}
}
