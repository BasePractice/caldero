//go:build integration

// Package testsupport поднимает PostgreSQL для интеграционных тестов.
// Собирается только с тегом integration: без него ни сам пакет, ни его
// зависимости в обычную сборку не попадают.
package testsupport

import (
	"context"
	"database/sql"
	"embed"
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
	redisImage    = "redis:7-alpine"
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

// PrepareRedis поднимает Redis и возвращает конфигурацию, указывающую
// на него. Отдельного модуля testcontainers для этого не требуется:
// образ поднимается обычным контейнером, а готовность определяется
// по строке в журнале.
func PrepareRedis(t *testing.T) services.Config {
	t.Helper()
	ctx := context.Background()

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        redisImage,
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor: wait.ForLog("Ready to accept connections").
				WithStartupTimeout(startupWait),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("не удалось поднять Redis: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Logf("не удалось остановить контейнер: %v", err)
		}
	})

	endpoint, err := container.PortEndpoint(ctx, "6379/tcp", "")
	if err != nil {
		t.Fatalf("не удалось получить адрес Redis: %v", err)
	}
	return services.Config{RedisURL: "redis://" + endpoint + "/0"}
}

//go:embed migrations/*.sql
var probeMigrations embed.FS

// ProbeMigrations отдаёт схему-заглушку. Нужна там, где проверяется сам
// механизм миграций, а не схема конкретного сервиса: NewDatabase принимает
// embed.FS, и директиву embed нельзя написать в пакете, где нет каталога
// migrations.
func ProbeMigrations() embed.FS {
	return probeMigrations
}
