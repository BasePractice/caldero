package account

import (
	"context"
	"database/sql"
	"embed"

	"wish/services"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Database interface {
	Create(ctx context.Context, account InputAccount, operator *services.AuthorizedUser) (*uuid.UUID, error)
	Get(ctx context.Context, id uuid.UUID) (*Account, error)
}

type ds struct {
	db *sql.DB
}

func (d ds) Create(ctx context.Context, account InputAccount, operator *services.AuthorizedUser) (*uuid.UUID, error) {
	//TODO implement me
	panic("implement me")
}

func (d ds) Get(ctx context.Context, id uuid.UUID) (*Account, error) {
	//TODO implement me
	panic("implement me")
}

func NewDatabase() Database {
	db, err := services.NewDatabase(migrations)
	if err != nil {
		return nil
	}
	return &ds{db}
}
