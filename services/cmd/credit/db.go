package main

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	"wish/services"

	_ "github.com/lib/pq"
)

//go:embed migrations/*.sql
var migrations embed.FS

type DatabaseCredit interface {
	CreateCredit(ctx context.Context, credit CreateCredit, operator *services.AuthorizedUser) (int64, error)
	GetCredit(ctx context.Context, id uint64) (*Credit, error)
}

type ds struct {
	db *sql.DB
}

func (d ds) GetCredit(ctx context.Context, id uint64) (*Credit, error) {
	var credit Credit

	err := d.db.QueryRowContext(ctx, `SELECT 
    user_id, creator_id, type, percent, balance, kind, month FROM credit WHERE id = $1`,
		id).Scan(&credit.UserId, &credit.CreatorId, &credit.Type, &credit.Percent, &credit.Balance, &credit.Kind, &credit.Month)
	if err != nil {
		return nil, err
	}
	return &credit, err
}

func (d ds) CreateCredit(ctx context.Context, credit CreateCredit, operator *services.AuthorizedUser) (int64, error) {
	var id int64
	if err := d.db.QueryRowContext(ctx, `
		INSERT INTO credit (user_id, creator_id, type, percent, balance, kind, month) 
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		credit.UserId, operator.Id, credit.Type, credit.Percent, credit.Balance, credit.Kind, credit.Month).Scan(&id); err != nil {
		return 0, fmt.Errorf("failed to create credit: %w", err)
	}
	return id, nil
}

func NewDatabaseCredit() DatabaseCredit {
	db, err := services.NewDatabase(migrations)
	if err != nil {
		return nil
	}
	return &ds{db}
}
