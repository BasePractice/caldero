package main

import (
	"database/sql"
	"embed"
	"fmt"
	"wish/services"

	_ "github.com/lib/pq"
)

//go:embed migrations/*.sql
var migrations embed.FS

type DatabaseCredit interface {
	CreateCredit(credit CreateCredit) (int64, error)
}

type ds struct {
	db *sql.DB
}

func (d ds) CreateCredit(credit CreateCredit) (int64, error) {
	var id int64
	if err := d.db.QueryRow("INSERT INTO credit (user_id, type, percent, balance) VALUES ($1, $2, $3, $4) RETURNING id",
		credit.UserId, credit.Type, credit.Percent, credit.Balance).Scan(&id); err != nil {
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
