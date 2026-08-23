package services

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// pingTimeout ограничивает проверку соединения при старте.
const pingTimeout = 5 * time.Second

func migrationScheme(db *sql.DB, migrations embed.FS) error {
	d, err := iofs.New(migrations, "migrations")
	if err != nil {
		return fmt.Errorf("opening migration resource: %w", err)
	}
	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		return fmt.Errorf("creating postgres migration driver: %w", err)
	}
	instance, err := migrate.NewWithInstance("iofs", d, "wish", driver)
	if err != nil {
		return fmt.Errorf("creating migration instance: %w", err)
	}
	if err = instance.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("applying migrations: %w", err)
	}

	version, dirty, err := instance.Version()
	if err != nil {
		return fmt.Errorf("reading schema version: %w", err)
	}
	slog.Info("Schema migrated", slog.Uint64("version", uint64(version)), slog.Bool("dirty", dirty))
	return nil
}

func NewDatabase(cfg Config, migrations embed.FS) (*sql.DB, error) {
	db, err := sql.Open("postgres", cfg.PostgresURL)
	if err != nil {
		return nil, fmt.Errorf("opening postgres connection: %w", err)
	}

	// Без ограничения пул растёт неограниченно и упирается в max_connections
	// уже на четырёх сервисах.
	db.SetMaxOpenConns(cfg.DBMaxOpenConns)
	db.SetMaxIdleConns(cfg.DBMaxIdleConns)
	db.SetConnMaxLifetime(cfg.DBConnMaxLifetime)

	// sql.Open соединение не устанавливает, поэтому недоступная база иначе
	// обнаруживается только на первом запросе.
	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()
	if err = db.PingContext(ctx); err != nil {
		_ = db.Close() // Соединение всё равно не установлено.
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}

	if err = migrationScheme(db, migrations); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}
