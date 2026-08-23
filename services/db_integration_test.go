//go:build integration

package services_test

import (
	"testing"
	"time"

	"wish/services"
	"wish/services/testsupport"
)

// TestNewDatabase проверяет сам механизм подключения и миграций: схема
// применяется при старте, и повторный запуск не должен ни падать, ни
// накатывать её второй раз.
func TestNewDatabase(t *testing.T) {
	ctx := t.Context()
	cfg := testsupport.Prepare(t, "probe")

	db, err := services.NewDatabase(ctx, cfg, testsupport.ProbeMigrations())
	if err != nil {
		t.Fatalf("подключение: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.ExecContext(ctx, `INSERT INTO probe (note) VALUES ($1)`, "проверка"); err != nil {
		t.Fatalf("схема не применена: %v", err)
	}

	// Ограничения пула берутся из конфигурации: без них пул растёт
	// неограниченно и упирается в max_connections уже на четырёх сервисах.
	if got := db.Stats().MaxOpenConnections; got != cfg.DBMaxOpenConns {
		t.Errorf("предел соединений %d, ожидался %d", got, cfg.DBMaxOpenConns)
	}

	t.Run("повторный старт не накатывает миграции второй раз", func(t *testing.T) {
		again, err := services.NewDatabase(ctx, cfg, testsupport.ProbeMigrations())
		if err != nil {
			t.Fatalf("повторное подключение: %v", err)
		}
		t.Cleanup(func() { _ = again.Close() })
	})
}

// TestNewDatabaseUnreachable: sql.Open соединение не устанавливает, поэтому
// недоступная база иначе обнаруживается только на первом запросе.
func TestNewDatabaseUnreachable(t *testing.T) {
	cfg := services.Config{
		PostgresURL:       "postgres://postgres:postgres@127.0.0.1:1/wish?sslmode=disable&connect_timeout=1",
		DBMaxOpenConns:    1,
		DBMaxIdleConns:    1,
		DBConnMaxLifetime: time.Minute,
	}

	if _, err := services.NewDatabase(t.Context(), cfg, testsupport.ProbeMigrations()); err == nil {
		t.Fatal("недоступная база принята")
	}
}

func TestNewDatabaseBadURL(t *testing.T) {
	cfg := services.Config{PostgresURL: "postgres://\x7f"}

	if _, err := services.NewDatabase(t.Context(), cfg, testsupport.ProbeMigrations()); err == nil {
		t.Fatal("некорректный адрес принят")
	}
}
