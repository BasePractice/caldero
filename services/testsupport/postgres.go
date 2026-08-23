//go:build integration

// Package testsupport поднимает PostgreSQL для интеграционных тестов.
// Собирается только с тегом integration: без него ни сам пакет, ни его
// зависимости в обычную сборку не попадают.
package testsupport

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"wish/services"

	_ "github.com/lib/pq"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	postgresImage = "postgres:16-alpine"
	startupWait   = 60 * time.Second
)

// Prepare поднимает PostgreSQL, создаёт схему сервиса и возвращает
// конфигурацию, указывающую на неё. Миграции применяет уже сам конструктор
// репозитория — то есть тест идёт тем же путём, что и рабочий сервис.
//
// Именно такие тесты поймали бы ошибки в SQL-строках: запрос по
// несуществующей колонке, синтаксис SQLite в PostgreSQL и порядок удаления
// таблиц компилятор не видит.
//
// Контейнер поднимается один на вызов, поэтому тесты пакета удобнее
// оформлять подтестами внутри одного Test-функции.
func Prepare(t *testing.T, schema string) services.Config {
	t.Helper()
	ctx := context.Background()

	container, err := postgres.Run(ctx, postgresImage,
		postgres.WithDatabase("wish"),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("postgres"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(startupWait)),
	)
	if err != nil {
		t.Fatalf("не удалось поднять PostgreSQL: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Logf("не удалось остановить контейнер: %v", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("не удалось получить строку подключения: %v", err)
	}

	// Схема создаётся отдельным подключением: миграции идут уже с search_path
	// на неё, как и в рабочем сервисе.
	bootstrap, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("не удалось подключиться: %v", err)
	}
	if _, err = bootstrap.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA %s", schema)); err != nil {
		t.Fatalf("не удалось создать схему %s: %v", schema, err)
	}
	if err = bootstrap.Close(); err != nil {
		t.Fatalf("не удалось закрыть подключение: %v", err)
	}

	return services.Config{
		PostgresURL:       dsn + "&search_path=" + schema,
		DBMaxOpenConns:    5,
		DBMaxIdleConns:    2,
		DBConnMaxLifetime: time.Minute,
	}
}
