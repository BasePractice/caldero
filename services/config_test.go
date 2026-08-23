package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// allKeys перечисляет переменные, которые читает LoadConfig. Тест умолчаний
// обязан убрать их из окружения: иначе он проверяет не значения по умолчанию,
// а .env разработчика.
var allKeys = []string{
	"DATABASE_URL", "REDIS_URL", "LOG_LEVEL", "LOG_FILE", "LOG_COLOR",
	"ADMIN_TOKEN", "OAUTH2_GLOBAL_SECRET", "OAUTH2_ISSUER", "OAUTH2_DEBUG",
	"KEY_MASTER_KEY", "KEY_ROTATION_MIN_INTERVAL", "TOKEN_CLEANUP_INTERVAL",
	"WALLET_ADDRESS", "MAX_IN_FLIGHT_REQUESTS", "RESERVATION_RELEASE_INTERVAL",
	"PARTITION_MAINTENANCE_INTERVAL", "PARTITION_MONTHS_AHEAD",
	"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_TRACES_SAMPLER_ARG",
	"DEBUG_PPROF", "DEBUG_STATSVIZ", "METRICS_PORT",
	"DB_MAX_OPEN_CONNS", "DB_MAX_IDLE_CONNS", "DB_CONN_MAX_LIFETIME",
	"PAYMENT_PROVIDER", "PAYMENT_WEBHOOK_SECRET", "PAYMENT_RECONCILE_INTERVAL",
	"PAYMENT_FEE_BASIS_POINTS", "PAYMENT_FEE_FIXED", "PAYMENT_FEE_MIN", "PAYMENT_FEE_MAX",
	"NOTIFY_TELEGRAM_TOKEN", "NOTIFY_TELEGRAM_BOT", "NOTIFY_TELEGRAM_API",
	"NOTIFY_WS_ORIGINS", "NOTIFY_BINDING_CODE_TTL", "NOTIFY_RATE_LIMIT",
	"NOTIFY_RATE_WINDOW", "NOTIFY_ENDPOINT", "SERVICE_USER_ID",
	"MARKETPLACE_PROVIDERS", "MARKETPLACE_CACHE_TTL", "MARKETPLACE_WALLET_ID",
	"WISHLIST_RESERVATION_TTL", "WISHLIST_RELEASE_INTERVAL",
	"FEE_WALLET_ID", "CALDRON_REFUND_INTERVAL",
	"CONFIRMATION_TTL", "CONFIRMATION_COOLDOWN",
	"CONFIRMATION_RATE_LIMIT", "CONFIRMATION_RATE_WINDOW",
	"PUBLIC_BASE_URL", "USERS_ENDPOINT",
	"SMTP_HOST", "SMTP_PORT", "SMTP_USERNAME", "SMTP_PASSWORD",
	"EMAIL_FROM", "EMAIL_UNSUBSCRIBE_URL", "EMAIL_SECRET",
	"WEB_API_BASE", "WEB_CLIENT_ID", "SOCIAL_PROVIDERS", "SOCIAL_REDIRECT_BASE",
}

// cleanEnv уводит тест в пустой каталог с пустым окружением: LoadConfig
// читает .env из текущего каталога, и без этого результат зависит от машины.
func cleanEnv(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
	for _, key := range allKeys {
		value, ok := os.LookupEnv(key)
		if !ok {
			continue
		}
		t.Cleanup(func() {
			if err := os.Setenv(key, value); err != nil {
				t.Errorf("восстановление %s: %v", key, err)
			}
		})
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("сброс %s: %v", key, err)
		}
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	cleanEnv(t)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("чтение конфигурации: %v", err)
	}

	tests := []struct {
		name string
		got  any
		want any
	}{
		{"уровень журнала", cfg.LogLevel, "INFO"},
		{"цветной вывод", cfg.LogColor, true},
		{"порт метрик", cfg.MetricsPort, 8081},
		{"предел одновременных запросов", cfg.MaxInFlightRequests, 256},
		{"партиций вперёд", cfg.PartitionMonthsAhead, 6},
		{"доля трассировок", cfg.OTelSampleRatio, 1.0},
		{"pprof выключен", cfg.DebugPprof, false},
		{"statsviz выключен", cfg.DebugStatsviz, false},
		{"соединений с базой", cfg.DBMaxOpenConns, 10},
		{"простаивающих соединений", cfg.DBMaxIdleConns, 5},
		{"время жизни соединения", cfg.DBConnMaxLifetime, 30 * time.Minute},
		{"платёжный провайдер", cfg.PaymentProvider, "SANDBOX"},
		{"интервал сверки платежей", cfg.PaymentReconcileInterval, 15 * time.Minute},
		{"срок кода привязки", cfg.NotifyBindingCodeTTL, 15 * time.Minute},
		{"частота оповещений", cfg.NotifyRateLimit, 10},
		{"окно частоты оповещений", cfg.NotifyRateWindow, time.Minute},
		{"срок резерва подарка", cfg.WishlistReservationTTL, 72 * time.Hour},
		{"интервал снятия резервов", cfg.WishlistReleaseInterval, 5 * time.Minute},
		{"срок кода подтверждения", cfg.ConfirmationTTL, 15 * time.Minute},
		{"пауза между кодами", cfg.ConfirmationCooldown, time.Minute},
		{"число кодов в окне", cfg.ConfirmationRateLimit, 5},
		{"окно кодов подтверждения", cfg.ConfirmationRateWindow, time.Hour},
		{"порт SMTP", cfg.SMTPPort, 587},
		{"клиент веб-интерфейса", cfg.WebClientId, "web"},
		{"кэш карточек площадок", cfg.MarketplaceCacheTTL, 10 * time.Minute},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.got != test.want {
				t.Errorf("получено %v, ожидалось %v", test.got, test.want)
			}
		})
	}

	// Площадки по умолчанию — заглушка: без неё список желаний не заведёт
	// ни одного товара, а внешних ключей в сборке по умолчанию нет.
	if len(cfg.MarketplaceProviders) != 1 || cfg.MarketplaceProviders[0] != "STUB" {
		t.Errorf("площадки %v, ожидалась заглушка", cfg.MarketplaceProviders)
	}
	// Секреты пустые по умолчанию: непустое значение здесь означало бы
	// зашитый в код ключ.
	for name, secret := range map[string]string{
		"ADMIN_TOKEN":            cfg.AdminToken,
		"OAUTH2_GLOBAL_SECRET":   cfg.OAuth2GlobalSecret,
		"KEY_MASTER_KEY":         cfg.KeyMasterKey,
		"PAYMENT_WEBHOOK_SECRET": cfg.PaymentWebhookSecret,
		"EMAIL_SECRET":           cfg.EmailSecret,
	} {
		if secret != "" {
			t.Errorf("%s по умолчанию не пуст", name)
		}
	}
	if cfg.ServiceUserId != uuid.Nil || cfg.FeeWalletId != uuid.Nil || cfg.MarketplaceWalletId != uuid.Nil {
		t.Error("идентификаторы кошельков по умолчанию должны быть нулевыми")
	}
	if cfg.NotifyWebSocketOrigins != nil || cfg.SocialProviders != nil {
		t.Error("пустой список должен разбираться в nil, а не в срез с пустой строкой")
	}
}

func TestLoadConfigFromEnvironment(t *testing.T) {
	cleanEnv(t)

	serviceUser := uuid.New()
	feeWallet := uuid.New()
	shopWallet := uuid.New()

	for key, value := range map[string]string{
		"DATABASE_URL":             "postgres://example/db",
		"LOG_LEVEL":                "DEBUG",
		"LOG_COLOR":                "false",
		"MAX_IN_FLIGHT_REQUESTS":   "512",
		"PARTITION_MONTHS_AHEAD":   "3",
		"OTEL_TRACES_SAMPLER_ARG":  "0.25",
		"DEBUG_PPROF":              "true",
		"DEBUG_STATSVIZ":           "true",
		"OAUTH2_DEBUG":             "true",
		"METRICS_PORT":             "9090",
		"PAYMENT_FEE_BASIS_POINTS": "150",
		"PAYMENT_FEE_FIXED":        "500",
		"PAYMENT_FEE_MIN":          "100",
		"PAYMENT_FEE_MAX":          "10000",
		"NOTIFY_WS_ORIGINS":        "https://a.example, https://b.example",
		"NOTIFY_RATE_LIMIT":        "0",
		"SERVICE_USER_ID":          serviceUser.String(),
		"FEE_WALLET_ID":            feeWallet.String(),
		"MARKETPLACE_WALLET_ID":    shopWallet.String(),
		"MARKETPLACE_PROVIDERS":    "OZON,WB",
		"WISHLIST_RESERVATION_TTL": "24h",
		"CONFIRMATION_RATE_LIMIT":  "1",
		"SMTP_PORT":                "465",
		"DB_CONN_MAX_LIFETIME":     "1h",
		"SOCIAL_PROVIDERS":         "yandex , vk",
	} {
		t.Setenv(key, value)
	}

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("чтение конфигурации: %v", err)
	}

	if cfg.PostgresURL != "postgres://example/db" || cfg.LogLevel != "DEBUG" || cfg.LogColor {
		t.Errorf("не применены базовые значения: %+v", cfg)
	}
	if cfg.MaxInFlightRequests != 512 || cfg.PartitionMonthsAhead != 3 || cfg.MetricsPort != 9090 {
		t.Errorf("не применены числовые значения: %+v", cfg)
	}
	if cfg.OTelSampleRatio != 0.25 {
		t.Errorf("доля трассировок %v, ожидалась 0.25", cfg.OTelSampleRatio)
	}
	if !cfg.DebugPprof || !cfg.DebugStatsviz || !cfg.OAuth2Debug {
		t.Errorf("не применены флаги отладки: %+v", cfg)
	}
	if cfg.PaymentFeeBasisPoints != 150 || cfg.PaymentFeeFixed != 500 ||
		cfg.PaymentFeeMin != 100 || cfg.PaymentFeeMax != 10000 {
		t.Errorf("не применены параметры комиссии: %+v", cfg)
	}
	// Нулевая частота — осмысленное значение: она выключает ограничение,
	// поэтому проверка «должно быть положительным» здесь была бы ошибкой.
	if cfg.NotifyRateLimit != 0 {
		t.Errorf("частота оповещений %d, ожидался ноль", cfg.NotifyRateLimit)
	}
	if cfg.ServiceUserId != serviceUser || cfg.FeeWalletId != feeWallet ||
		cfg.MarketplaceWalletId != shopWallet {
		t.Errorf("не применены идентификаторы: %+v", cfg)
	}
	if cfg.WishlistReservationTTL != 24*time.Hour || cfg.DBConnMaxLifetime != time.Hour {
		t.Errorf("не применены длительности: %+v", cfg)
	}
	if cfg.ConfirmationRateLimit != 1 || cfg.SMTPPort != 465 {
		t.Errorf("не применены значения подтверждений и почты: %+v", cfg)
	}

	// Пробелы вокруг элементов списка убираются: строка приходит из .env,
	// и «yandex , vk» — обычный способ её записать.
	want := []string{"yandex", "vk"}
	if len(cfg.SocialProviders) != len(want) {
		t.Fatalf("провайдеры входа %v, ожидались %v", cfg.SocialProviders, want)
	}
	for i, provider := range want {
		if cfg.SocialProviders[i] != provider {
			t.Errorf("провайдер %q, ожидался %q", cfg.SocialProviders[i], provider)
		}
	}
	if len(cfg.NotifyWebSocketOrigins) != 2 || cfg.NotifyWebSocketOrigins[1] != "https://b.example" {
		t.Errorf("источники WebSocket %v", cfg.NotifyWebSocketOrigins)
	}
	if len(cfg.MarketplaceProviders) != 2 || cfg.MarketplaceProviders[0] != "OZON" {
		t.Errorf("площадки %v", cfg.MarketplaceProviders)
	}
}

// TestLoadConfigInvalid проходит по каждой проверке значения. Неверная
// настройка обязана ронять старт с названием переменной: сервис, молча
// поднявшийся с нулевым таймаутом, ищут потом сутки.
func TestLoadConfigInvalid(t *testing.T) {
	tests := []struct {
		key   string
		value string
	}{
		{"MAX_IN_FLIGHT_REQUESTS", "много"},
		{"RESERVATION_RELEASE_INTERVAL", "часок"},
		{"PARTITION_MAINTENANCE_INTERVAL", "часок"},
		{"PARTITION_MONTHS_AHEAD", "полтора"},
		{"PARTITION_MONTHS_AHEAD", "0"},
		{"OTEL_TRACES_SAMPLER_ARG", "половина"},
		{"OTEL_TRACES_SAMPLER_ARG", "-0.1"},
		{"OTEL_TRACES_SAMPLER_ARG", "1.1"},
		{"DEBUG_PPROF", "ага"},
		{"DEBUG_STATSVIZ", "ага"},
		{"OAUTH2_DEBUG", "ага"},
		{"TOKEN_CLEANUP_INTERVAL", "иногда"},
		{"KEY_ROTATION_MIN_INTERVAL", "иногда"},
		{"LOG_COLOR", "ага"},
		{"METRICS_PORT", "восемь"},
		{"DB_MAX_OPEN_CONNS", "десять"},
		{"DB_MAX_IDLE_CONNS", "пять"},
		{"PAYMENT_FEE_BASIS_POINTS", "полтора"},
		{"PAYMENT_FEE_BASIS_POINTS", "-1"},
		{"PAYMENT_FEE_FIXED", "-1"},
		{"PAYMENT_FEE_MIN", "-1"},
		{"PAYMENT_FEE_MAX", "-1"},
		{"PAYMENT_RECONCILE_INTERVAL", "иногда"},
		{"NOTIFY_BINDING_CODE_TTL", "иногда"},
		{"NOTIFY_RATE_LIMIT", "десять"},
		{"NOTIFY_RATE_LIMIT", "-1"},
		{"NOTIFY_RATE_WINDOW", "иногда"},
		{"SERVICE_USER_ID", "не-uuid"},
		{"MARKETPLACE_CACHE_TTL", "иногда"},
		{"WISHLIST_RESERVATION_TTL", "иногда"},
		{"WISHLIST_RESERVATION_TTL", "0s"},
		{"WISHLIST_RELEASE_INTERVAL", "иногда"},
		{"MARKETPLACE_WALLET_ID", "не-uuid"},
		{"FEE_WALLET_ID", "не-uuid"},
		{"CALDRON_REFUND_INTERVAL", "иногда"},
		{"CONFIRMATION_TTL", "иногда"},
		{"CONFIRMATION_COOLDOWN", "иногда"},
		{"CONFIRMATION_RATE_LIMIT", "пять"},
		{"CONFIRMATION_RATE_LIMIT", "0"},
		{"CONFIRMATION_RATE_WINDOW", "иногда"},
		{"SMTP_PORT", "пятьсот"},
		{"DB_CONN_MAX_LIFETIME", "иногда"},
	}

	for _, test := range tests {
		t.Run(test.key+"="+test.value, func(t *testing.T) {
			cleanEnv(t)
			t.Setenv(test.key, test.value)

			_, err := LoadConfig()
			if err == nil {
				t.Fatalf("значение %q принято", test.value)
			}
			if !strings.Contains(err.Error(), test.key) {
				t.Errorf("ошибка %q не называет переменную %s", err, test.key)
			}
		})
	}
}

// TestLoadConfigEnvFile проверяет порядок источников: .env.local перекрывает
// .env, а настоящее окружение — оба файла. Порядок неочевиден, потому что
// godotenv уже установленные переменные не перезаписывает.
func TestLoadConfigEnvFile(t *testing.T) {
	cleanEnv(t)
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("текущий каталог: %v", err)
	}

	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("запись %s: %v", name, err)
		}
	}
	write(".env", "LOG_LEVEL=WARN\nADMIN_TOKEN=from-env\nOAUTH2_ISSUER=https://env\n")
	write(".env.local", "LOG_LEVEL=DEBUG\nADMIN_TOKEN=from-local\n")
	t.Setenv("LOG_LEVEL", "ERROR")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("чтение конфигурации: %v", err)
	}

	if cfg.LogLevel != "ERROR" {
		t.Errorf("уровень журнала %q, окружение должно перекрывать файлы", cfg.LogLevel)
	}
	if cfg.AdminToken != "from-local" {
		t.Errorf("токен %q, .env.local должен перекрывать .env", cfg.AdminToken)
	}
	if cfg.OAuth2Issuer != "https://env" {
		t.Errorf("издатель %q, значение из .env потеряно", cfg.OAuth2Issuer)
	}
}

func TestLoadConfigBrokenEnvFile(t *testing.T) {
	cleanEnv(t)
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("текущий каталог: %v", err)
	}
	// Каталог вместо файла: чтение падает не с fs.ErrNotExist, и такую
	// ошибку прятать нельзя — иначе конфигурация тихо не применится.
	if err := os.Mkdir(filepath.Join(dir, ".env"), 0o700); err != nil {
		t.Fatalf("создание каталога .env: %v", err)
	}

	if _, err := LoadConfig(); err == nil {
		t.Fatal("нечитаемый .env принят")
	}
}
