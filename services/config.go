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

		AdminToken: env("ADMIN_TOKEN", ""),
	}

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

	return cfg, nil
}

func env(key, defaultValue string) string {
	v, ok := os.LookupEnv(key)
	if !ok {
		return defaultValue
	}
	return v
}
