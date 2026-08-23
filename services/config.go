package services

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
)

// Config — конфигурация сервиса. Читается один раз при старте, дальше не меняется.
type Config struct {
	PostgresURL string
	RedisURL    string
	LogLevel    string
	LogFile     string
	LogColor    bool
	MetricsPort int

	// AdminToken защищает служебные эндпоинты. Пустое значение полностью
	// отключает их: незакрытая ротация ключей — это и инвалидация всех
	// выданных токенов, и генерация RSA-2048 на каждый запрос.
	AdminToken string
	// KeyRotationMinInterval ограничивает частоту ротации ключей.
	KeyRotationMinInterval time.Duration

	// OAuth2GlobalSecret — секрет подписи токенов, ровно 32 байта.
	// Пустое значение допустимо только для локальной разработки.
	OAuth2GlobalSecret string
	// OAuth2Debug включает передачу внутренних деталей ошибок клиенту.
	OAuth2Debug bool
	// OAuth2Issuer попадает в claim iss выданных токенов.
	OAuth2Issuer string
	// KeyMasterKey шифрует приватные ключи подписи в БД. Пустое значение
	// оставляет их открытым текстом — допустимо только для локального стенда.
	KeyMasterKey string

	// Пул соединений. Суммарно по всем сервисам должен укладываться
	// в max_connections PostgreSQL, по умолчанию равный 100.
	DBMaxOpenConns    int
	DBMaxIdleConns    int
	DBConnMaxLifetime time.Duration

	// TokenCleanupInterval — как часто удалять просроченные токены.
	// Нулевое значение отключает очистку.
	TokenCleanupInterval time.Duration

	// MaxInFlightRequests ограничивает число одновременно обрабатываемых
	// запросов. Ноль снимает ограничение.
	MaxInFlightRequests int

	// PartitionMaintenanceInterval — как часто проверять окно партиций
	// транзакций. Ноль отключает обслуживание.
	PartitionMaintenanceInterval time.Duration
	// PartitionMonthsAhead — на сколько месяцев вперёд держать партиции.
	PartitionMonthsAhead int

	// OTelEndpoint — адрес приёмника трасс по OTLP/HTTP.
	// Пустое значение отключает трассировку.
	OTelEndpoint string
	// OTelSampleRatio — доля трасс, которые выгружаются.
	OTelSampleRatio float64

	// DebugStatsviz открывает страницу состояния рантайма на порту метрик.
	// По умолчанию выключено: страница не аутентифицирована.
	DebugStatsviz bool
}

// LoadConfig читает .env-файлы и окружение. Вызывается первой строкой main:
// раньше значения раскладывались по пакетным переменным при инициализации
// пакета, то есть до вызова godotenv.Load, и содержимое .env не применялось
// вообще.
func LoadConfig() (Config, error) {
	// Порядок важен: godotenv не перезаписывает уже установленные переменные,
	// поэтому .env.local читается первым и перекрывает .env, а настоящее
	// окружение перекрывает оба файла.
	for _, name := range []string{".env.local", ".env"} {
		if err := godotenv.Load(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return Config{}, fmt.Errorf("loading %s: %w", name, err)
		}
		// Отсутствие файла — штатная ситуация: в контейнере конфигурация
		// приходит через переменные окружения.
	}

	cfg := Config{
		PostgresURL: env("DATABASE_URL",
			"postgres://postgres:postgres@localhost:25432/wish?sslmode=disable&search_path=public"),
		RedisURL: env("REDIS_URL", "redis://localhost:6379/10?protocol=3"),
		LogLevel: env("LOG_LEVEL", "INFO"),
		LogFile:  env("LOG_FILE", ""),

		AdminToken:         env("ADMIN_TOKEN", ""),
		OAuth2GlobalSecret: env("OAUTH2_GLOBAL_SECRET", ""),
		OAuth2Issuer:       env("OAUTH2_ISSUER", "http://localhost:8080/api/v1"),
		KeyMasterKey:       env("KEY_MASTER_KEY", ""),
	}

	maxInFlight, err := strconv.Atoi(env("MAX_IN_FLIGHT_REQUESTS", "256"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing MAX_IN_FLIGHT_REQUESTS: %w", err)
	}
	cfg.MaxInFlightRequests = maxInFlight

	partitionInterval, err := time.ParseDuration(env("PARTITION_MAINTENANCE_INTERVAL", "12h"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing PARTITION_MAINTENANCE_INTERVAL: %w", err)
	}
	cfg.PartitionMaintenanceInterval = partitionInterval

	monthsAhead, err := strconv.Atoi(env("PARTITION_MONTHS_AHEAD", "6"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing PARTITION_MONTHS_AHEAD: %w", err)
	}
	if monthsAhead < 1 {
		return Config{}, fmt.Errorf("PARTITION_MONTHS_AHEAD must be positive, got %d", monthsAhead)
	}
	cfg.PartitionMonthsAhead = monthsAhead

	cfg.OTelEndpoint = env("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	sampleRatio, err := strconv.ParseFloat(env("OTEL_TRACES_SAMPLER_ARG", "1.0"), 64)
	if err != nil {
		return Config{}, fmt.Errorf("parsing OTEL_TRACES_SAMPLER_ARG: %w", err)
	}
	if sampleRatio < 0 || sampleRatio > 1 {
		return Config{}, fmt.Errorf("OTEL_TRACES_SAMPLER_ARG must be between 0 and 1, got %v", sampleRatio)
	}
	cfg.OTelSampleRatio = sampleRatio

	debugStatsviz, err := strconv.ParseBool(env("DEBUG_STATSVIZ", "false"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing DEBUG_STATSVIZ: %w", err)
	}
	cfg.DebugStatsviz = debugStatsviz

	oauth2Debug, err := strconv.ParseBool(env("OAUTH2_DEBUG", "false"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing OAUTH2_DEBUG: %w", err)
	}
	cfg.OAuth2Debug = oauth2Debug

	cleanupInterval, err := time.ParseDuration(env("TOKEN_CLEANUP_INTERVAL", "15m"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing TOKEN_CLEANUP_INTERVAL: %w", err)
	}
	cfg.TokenCleanupInterval = cleanupInterval

	rotationInterval, err := time.ParseDuration(env("KEY_ROTATION_MIN_INTERVAL", "1h"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing KEY_ROTATION_MIN_INTERVAL: %w", err)
	}
	cfg.KeyRotationMinInterval = rotationInterval

	logColor, err := strconv.ParseBool(env("LOG_COLOR", "true"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing LOG_COLOR: %w", err)
	}
	cfg.LogColor = logColor

	metricsPort, err := strconv.Atoi(env("METRICS_PORT", "8081"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing METRICS_PORT: %w", err)
	}
	cfg.MetricsPort = metricsPort

	maxOpen, err := strconv.Atoi(env("DB_MAX_OPEN_CONNS", "10"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing DB_MAX_OPEN_CONNS: %w", err)
	}
	cfg.DBMaxOpenConns = maxOpen

	maxIdle, err := strconv.Atoi(env("DB_MAX_IDLE_CONNS", "5"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing DB_MAX_IDLE_CONNS: %w", err)
	}
	cfg.DBMaxIdleConns = maxIdle

	connLifetime, err := time.ParseDuration(env("DB_CONN_MAX_LIFETIME", "30m"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing DB_CONN_MAX_LIFETIME: %w", err)
	}
	cfg.DBConnMaxLifetime = connLifetime

	return cfg, nil
}

func env(key, defaultValue string) string {
	v, ok := os.LookupEnv(key)
	if !ok {
		return defaultValue
	}
	return v
}
