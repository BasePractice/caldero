package services

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

const (
	// pingTimeout ограничивает проверку соединения при старте.
	pingTimeout = 5 * time.Second
	// migrationTimeout ограничивает шаг миграции: зависшая блокировка
	// иначе держала бы доставку бесконечно.
	migrationTimeout = 5 * time.Minute
)

// ErrSchemaBehind — схема в базе отстаёт от кода или осталась в незавершённом
// состоянии. Отдельная ошибка нужна, чтобы сервис останавливался с понятной
// причиной: работать по старой схеме он всё равно не может, а сообщение
// «нет колонки» вместо этого пришло бы первому же запросу пользователя.
var ErrSchemaBehind = errors.New("database schema is behind the code")

func migrationScheme(db *sql.DB, migrations embed.FS) error {
	instance, err := migrator(db, migrations)
	if err != nil {
		return err
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

// verifyScheme проверяет, что схема доведена до версии, которую знает код.
//
// Проверка нужна там, где миграции применяет доставка: без неё сервис
// со свежим кодом молча поднялся бы на старой схеме и падал бы на запросах.
func verifyScheme(db *sql.DB, migrations embed.FS) error {
	expected, err := latestMigration(migrations)
	if err != nil {
		return err
	}

	instance, err := migrator(db, migrations)
	if err != nil {
		return err
	}
	version, dirty, err := instance.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return fmt.Errorf("%w: schema is empty, expected version %d", ErrSchemaBehind, expected)
	}
	if err != nil {
		return fmt.Errorf("reading schema version: %w", err)
	}
	if dirty {
		// Незавершённая миграция: часть изменений применена, часть нет.
		// Дальше идти нельзя — состояние схемы неизвестно.
		return fmt.Errorf("%w: schema version %d is dirty", ErrSchemaBehind, version)
	}
	if uint64(version) < expected {
		return fmt.Errorf("%w: schema version %d, code expects %d", ErrSchemaBehind, version, expected)
	}

	// Версия выше ожидаемой — это откат кода при неоткаченной схеме.
	// Останавливать сервис незачем: миграции пишутся совместимыми вперёд,
	// но знать об этом нужно.
	if uint64(version) > expected {
		slog.Warn("Database schema is newer than the code",
			slog.Uint64("version", uint64(version)), slog.Uint64("expected", expected))
	}
	slog.Info("Schema verified", slog.Uint64("version", uint64(version)))
	return nil
}

func migrator(db *sql.DB, migrations embed.FS) (*migrate.Migrate, error) {
	source, err := iofs.New(migrations, "migrations")
	if err != nil {
		return nil, fmt.Errorf("opening migration resource: %w", err)
	}
	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		return nil, fmt.Errorf("creating postgres migration driver: %w", err)
	}
	instance, err := migrate.NewWithInstance("iofs", source, "wish", driver)
	if err != nil {
		return nil, fmt.Errorf("creating migration instance: %w", err)
	}
	return instance, nil
}

// latestMigration — наибольшая версия среди вложенных в бинарник миграций.
// Версия берётся из числового префикса имени файла, как её понимает
// golang-migrate.
//
// Принимает fs.FS, а не embed.FS: разбор имён — это чистая логика,
// и проверять её на настоящем каталоге незачем.
func latestMigration(migrations fs.FS) (uint64, error) {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return 0, fmt.Errorf("reading migrations: %w", err)
	}

	var latest uint64
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || path.Ext(name) != ".sql" {
			continue
		}
		prefix, _, found := strings.Cut(name, "_")
		if !found {
			continue
		}
		version, err := strconv.ParseUint(prefix, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parsing version of migration %s: %w", name, err)
		}
		if version > latest {
			latest = version
		}
	}
	if latest == 0 {
		return 0, errors.New("no migrations found")
	}
	return latest, nil
}

// openDatabase открывает пул и убеждается, что база отвечает.
func openDatabase(ctx context.Context, cfg Config) (*sql.DB, error) {
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
	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err = db.PingContext(pingCtx); err != nil {
		_ = db.Close() // Соединение всё равно не установлено.
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	return db, nil
}

func NewDatabase(ctx context.Context, cfg Config, migrations embed.FS) (*sql.DB, error) {
	db, err := openDatabase(ctx, cfg)
	if err != nil {
		return nil, err
	}

	// Схема либо доводится до нужной версии здесь, либо уже доведена
	// шагом доставки — и тогда остаётся её проверить.
	apply := migrationScheme
	if !cfg.DBMigrate {
		apply = verifyScheme
	}
	if err = apply(db, migrations); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// Migrate применяет миграции и завершает процесс. Служит отдельным шагом
// доставки: образ сервиса несёт свои миграции в себе, поэтому применяет
// их он же — запуском с флагом -migrate.
//
// Код возврата отличает успех от отказа: доставка обязана остановиться
// на неудачной миграции, не подменяя образы. Настройки берутся из окружения
// тем же кодом, что и у сервиса: шаг доставки обязан ходить в ту же базу.
func Migrate(name string, migrations embed.FS) {
	if err := ApplyMigrations(name, migrations); err != nil {
		fmt.Fprintln(os.Stderr, "migration failed:", err)
		os.Exit(1)
	}
	slog.Info("Migrations applied", slog.String("service", name))
}

// ApplyMigrations выполняет ту же работу, что и Migrate, но возвращает
// ошибку вместо кода возврата.
func ApplyMigrations(name string, migrations embed.FS) error {
	cfg, err := LoadConfig()
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}
	if _, err = DefineLogging(cfg); err != nil {
		return fmt.Errorf("configuring logging: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), migrationTimeout)
	defer cancel()

	db, err := openDatabase(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		// Соединение всё равно закрывается концом процесса; закрытие
		// здесь нужно, чтобы блокировка миграции снялась сразу.
		_ = db.Close()
	}()

	if err = migrationScheme(db, migrations); err != nil {
		return fmt.Errorf("service %s: %w", name, err)
	}
	return nil
}
