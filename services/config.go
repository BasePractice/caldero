package services

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
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

	// DBMigrate разрешает сервису применять миграции при старте.
	//
	// В доставке миграции применяются отдельным шагом, до подмены
	// образов, и сервису остаётся только проверить, что схема не отстаёт
	// от кода: сбой миграции должен останавливать выкат целиком,
	// а не заставать половину сервисов уже перезапущенными.
	// Локальный стенд поднимается одной командой, поэтому по умолчанию
	// сервис применяет миграции сам.
	DBMigrate bool

	// Пул соединений. Суммарно по всем сервисам должен укладываться
	// в max_connections PostgreSQL, по умолчанию равный 100.
	DBMaxOpenConns    int
	DBMaxIdleConns    int
	DBConnMaxLifetime time.Duration

	// TokenCleanupInterval — как часто удалять просроченные токены.
	// Нулевое значение отключает очистку.
	TokenCleanupInterval time.Duration

	// WalletAddress — адрес сервиса кошелька. Пустое значение отключает
	// операции, которым он нужен.
	WalletAddress string

	// MaxInFlightRequests ограничивает число одновременно обрабатываемых
	// запросов. Ноль снимает ограничение.
	MaxInFlightRequests int

	// ReservationReleaseInterval — как часто освобождать просроченные
	// резервы. Ноль отключает освобождение.
	ReservationReleaseInterval time.Duration

	// PartitionMaintenanceInterval — как часто проверять окно партиций
	// транзакций. Ноль отключает обслуживание.
	PartitionMaintenanceInterval time.Duration
	// PartitionMonthsAhead — на сколько месяцев вперёд держать партиции.
	PartitionMonthsAhead int
	// TransactionRetentionMonths — сколько месяцев история операций
	// остаётся в основной таблице. Партиции старше отсоединяются,
	// но не удаляются: удаление финансовой истории необратимо и делается
	// человеком, а не расписанием. Ноль отключает отсоединение.
	TransactionRetentionMonths int

	// OTelEndpoint — адрес приёмника трасс по OTLP/HTTP.
	// Пустое значение отключает трассировку.
	OTelEndpoint string
	// OTelSampleRatio — доля трасс, которые выгружаются.
	OTelSampleRatio float64

	// DebugPprof открывает профилировщик на служебном порту.
	// Профили раскрываютвнутреннее устройство сервиса, поэтому по умолчанию выключен.
	DebugPprof bool

	// PaymentProvider выбирает платёжного провайдера. Значение SANDBOX
	// означает песочницу: приём и вывод настоящих денег требует статуса
	// платёжного агента или договора с ним (EXT-01).
	PaymentProvider string
	// PaymentWebhookSecret — общий секрет подписи вебхуков провайдера.
	// Пустое значение отключает приём вебхуков: непроверенный вебхук —
	// это возможность зачислить себе любую сумму запросом снаружи.
	PaymentWebhookSecret string
	// Комиссия платёжного контура. Тариф у каждого провайдера свой
	// и меняется без изменения кода, поэтому живёт в конфигурации.
	// Доля — в базисных пунктах, суммы — в копейках.
	PaymentFeeBasisPoints int64
	PaymentFeeFixed       int64
	PaymentFeeMin         int64
	PaymentFeeMax         int64
	// PaymentReconcileInterval — как часто сверять незавершённые операции
	// с провайдером. Ноль отключает сверку, и тогда потерянный вебхук
	// оставляет операцию незавершённой навсегда.
	PaymentReconcileInterval time.Duration

	// NotifyTelegramToken — токен бота. Пустое значение выключает канал
	// Telegram целиком: без токена бот не может ни отправить сообщение,
	// ни принять привязку.
	NotifyTelegramToken string
	// NotifyTelegramBot — имя бота для ссылки привязки вида t.me/бот.
	NotifyTelegramBot string
	// NotifyTelegramAPI — база адресов Bot API. Задаётся только стендом,
	// где вместо Telegram отвечает заглушка.
	NotifyTelegramAPI string
	// NotifyWebSocketOrigins — источники, которым разрешено открывать
	// WebSocket. Пустой список оставляет проверку по умолчанию: источник
	// должен совпадать с хостом, то есть браузерный клиент со своего
	// домена подключиться не сможет, пока домен не указан здесь.
	NotifyWebSocketOrigins []string
	// NotifyBindingCodeTTL — сколько живёт код привязки мессенджера.
	NotifyBindingCodeTTL time.Duration
	// NotifyRateLimit и NotifyRateWindow ограничивают число сообщений
	// одному пользователю в один канал за окно. Ноль снимает ограничение.
	NotifyRateLimit  int
	NotifyRateWindow time.Duration

	// ServiceUserId — от чьего имени сервисы вызывают друг друга там, где
	// нужна роль оператора: публикация оповещения чужому пользователю,
	// чтение чужого кошелька. Это не человек, а идентификатор источника
	// в логах; границу держит то, что порты сервисов не опубликованы.
	ServiceUserId uuid.UUID
	// NotifyEndpoint — адрес сервиса оповещений. Пустое значение выключает
	// оповещения: операция проходит, сообщение не отправляется.
	NotifyEndpoint string

	// MarketplaceProviders — подключённые торговые площадки. STUB означает
	// заглушку для локального стенда: доступа к API площадок нет (ADR 0004).
	MarketplaceProviders []string
	// MarketplaceCacheTTL — сколько живёт карточка товара в кэше.
	MarketplaceCacheTTL time.Duration

	// WishlistReservationTTL — на сколько подарок резервируется за дарителем.
	// Без срока брошенный резерв блокирует подарок навсегда.
	WishlistReservationTTL time.Duration
	// WishlistReleaseInterval — как часто освобождать просроченные резервы.
	// Ноль отключает освобождение.
	WishlistReleaseInterval time.Duration
	// MarketplaceWalletId — кошелёк площадки: покупка это уход средств
	// из системы к продавцу, и у этого ухода должен быть адресат. Пустое
	// значение выключает покупки: списывать «в никуда» значило бы нарушить
	// сходимость средств в системе.
	MarketplaceWalletId uuid.UUID
	// FeeWalletId — кошелёк, на который удерживается комиссия. Пустое
	// значение означает, что комиссия не удерживается: списывать её
	// «в никуда» значило бы нарушить сходимость средств в системе.
	FeeWalletId uuid.UUID

	// CaldronRefundInterval — как часто добивать незавершённые возвраты
	// по отменённым котлам. Ноль отключает добивание, и тогда сбой посреди
	// отмены оставляет средства участников в котле.
	CaldronRefundInterval time.Duration

	// ConfirmationTTL — сколько живёт код подтверждения контакта.
	ConfirmationTTL time.Duration
	// ConfirmationCooldown — минимальная пауза между отправками кода
	// одному пользователю. Без неё эндпоинт отправки превращается
	// в средство рассылки за чужой счёт.
	ConfirmationCooldown time.Duration
	// ConfirmationRateLimit и ConfirmationRateWindow ограничивают число
	// кодов за окно.
	ConfirmationRateLimit  int
	ConfirmationRateWindow time.Duration
	// PublicBaseURL — адрес, по которому система доступна снаружи.
	// Из него собирается ссылка подтверждения почты; пустое значение
	// означает, что ссылку показать негде и в письмо уйдёт код.
	PublicBaseURL string

	// SocialProviders — имена подключённых внешних провайдеров входа.
	// Их настройки читает сам сервис пользователей: набор переменных
	// зависит от числа провайдеров и нужен только там.
	SocialProviders []string
	// SocialRedirectBase — база адресов возврата от провайдера. Задаётся
	// явно, потому что адрес обязан совпадать с зарегистрированным
	// у провайдера, а заголовок Host запроса подделывается.
	SocialRedirectBase string

	// WebAPIBase — адрес API для веб-интерфейса. Отдаётся браузеру
	// отдельным ответом, а не зашивается в бандл: один и тот же файл
	// должен работать и на стенде, и в бою.
	WebAPIBase string
	// WebClientId — идентификатор публичного клиента OAuth2, от имени
	// которого входит интерфейс.
	WebClientId string

	// UsersEndpoint — адрес сервиса пользователей. Сервису оповещений
	// он нужен, чтобы узнать адрес почты: держать вторую копию контактов
	// значит разойтись с профилем при первой же смене адреса.
	UsersEndpoint string
	// Отправка писем. Пустой SMTPHost выключает канал целиком.
	SMTPHost     string
	SMTPPort     int
	SMTPUsername string
	SMTPPassword string
	// EmailFrom — адрес отправителя. Для него должны быть настроены SPF,
	// DKIM и DMARC, иначе письма уходят в спам и канал бесполезен.
	EmailFrom string
	// EmailUnsubscribeURL — база ссылки отписки. Рассылка без отписки
	// нарушает правила почтовых провайдеров, поэтому пустое значение
	// выключает канал.
	EmailUnsubscribeURL string
	// EmailSecret подписывает ссылку отписки: без подписи достаточно
	// подставить чужой идентификатор, чтобы отписать постороннего.
	EmailSecret string

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

	cfg.WalletAddress = env("WALLET_ADDRESS", "")

	maxInFlight, err := strconv.Atoi(env("MAX_IN_FLIGHT_REQUESTS", "256"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing MAX_IN_FLIGHT_REQUESTS: %w", err)
	}
	cfg.MaxInFlightRequests = maxInFlight

	releaseInterval, err := time.ParseDuration(env("RESERVATION_RELEASE_INTERVAL", "1m"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing RESERVATION_RELEASE_INTERVAL: %w", err)
	}
	cfg.ReservationReleaseInterval = releaseInterval

	partitionInterval, err := time.ParseDuration(env("PARTITION_MAINTENANCE_INTERVAL", "12h"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing PARTITION_MAINTENANCE_INTERVAL: %w", err)
	}
	cfg.PartitionMaintenanceInterval = partitionInterval

	// Шесть лет, а не пять: закон о бухучёте требует хранить первичные
	// документы не менее пяти лет после отчётного года, а отсчёт здесь
	// помесячный — операция января попадает под срок до конца пятого года
	// после своего, то есть почти шесть лет от самой операции.
	retention, err := strconv.Atoi(env("TRANSACTION_RETENTION_MONTHS", "72"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing TRANSACTION_RETENTION_MONTHS: %w", err)
	}
	if retention < 0 {
		return Config{}, fmt.Errorf("TRANSACTION_RETENTION_MONTHS must not be negative, got %d", retention)
	}
	cfg.TransactionRetentionMonths = retention

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

	debugPprof, err := strconv.ParseBool(env("DEBUG_PPROF", "false"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing DEBUG_PPROF: %w", err)
	}
	cfg.DebugPprof = debugPprof

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

	dbMigrate, err := strconv.ParseBool(env("DB_MIGRATE", "true"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing DB_MIGRATE: %w", err)
	}
	cfg.DBMigrate = dbMigrate

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

	cfg.PaymentProvider = env("PAYMENT_PROVIDER", "SANDBOX")
	cfg.PaymentWebhookSecret = env("PAYMENT_WEBHOOK_SECRET", "")

	for _, fee := range []struct {
		key   string
		value *int64
	}{
		{"PAYMENT_FEE_BASIS_POINTS", &cfg.PaymentFeeBasisPoints},
		{"PAYMENT_FEE_FIXED", &cfg.PaymentFeeFixed},
		{"PAYMENT_FEE_MIN", &cfg.PaymentFeeMin},
		{"PAYMENT_FEE_MAX", &cfg.PaymentFeeMax},
	} {
		parsed, err := strconv.ParseInt(env(fee.key, "0"), 10, 64)
		if err != nil {
			return Config{}, fmt.Errorf("parsing %s: %w", fee.key, err)
		}
		if parsed < 0 {
			return Config{}, fmt.Errorf("%s must not be negative, got %d", fee.key, parsed)
		}
		*fee.value = parsed
	}

	reconcileInterval, err := time.ParseDuration(env("PAYMENT_RECONCILE_INTERVAL", "15m"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing PAYMENT_RECONCILE_INTERVAL: %w", err)
	}
	cfg.PaymentReconcileInterval = reconcileInterval

	cfg.NotifyTelegramToken = env("NOTIFY_TELEGRAM_TOKEN", "")
	cfg.NotifyTelegramBot = env("NOTIFY_TELEGRAM_BOT", "")
	cfg.NotifyTelegramAPI = env("NOTIFY_TELEGRAM_API", "")
	cfg.NotifyWebSocketOrigins = splitList(env("NOTIFY_WS_ORIGINS", ""))

	bindingTTL, err := time.ParseDuration(env("NOTIFY_BINDING_CODE_TTL", "15m"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing NOTIFY_BINDING_CODE_TTL: %w", err)
	}
	cfg.NotifyBindingCodeTTL = bindingTTL

	rateLimit, err := strconv.Atoi(env("NOTIFY_RATE_LIMIT", "10"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing NOTIFY_RATE_LIMIT: %w", err)
	}
	if rateLimit < 0 {
		return Config{}, fmt.Errorf("NOTIFY_RATE_LIMIT must not be negative, got %d", rateLimit)
	}
	cfg.NotifyRateLimit = rateLimit

	rateWindow, err := time.ParseDuration(env("NOTIFY_RATE_WINDOW", "1m"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing NOTIFY_RATE_WINDOW: %w", err)
	}
	cfg.NotifyRateWindow = rateWindow

	if id := env("SERVICE_USER_ID", ""); id != "" {
		serviceUser, err := uuid.Parse(id)
		if err != nil {
			return Config{}, fmt.Errorf("parsing SERVICE_USER_ID: %w", err)
		}
		cfg.ServiceUserId = serviceUser
	}
	cfg.NotifyEndpoint = env("NOTIFY_ENDPOINT", "")

	cfg.MarketplaceProviders = splitList(env("MARKETPLACE_PROVIDERS", "STUB"))

	marketplaceTTL, err := time.ParseDuration(env("MARKETPLACE_CACHE_TTL", "10m"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing MARKETPLACE_CACHE_TTL: %w", err)
	}
	cfg.MarketplaceCacheTTL = marketplaceTTL

	reservationTTL, err := time.ParseDuration(env("WISHLIST_RESERVATION_TTL", "72h"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing WISHLIST_RESERVATION_TTL: %w", err)
	}
	if reservationTTL <= 0 {
		return Config{}, fmt.Errorf("WISHLIST_RESERVATION_TTL must be positive, got %s", reservationTTL)
	}
	cfg.WishlistReservationTTL = reservationTTL

	wishlistRelease, err := time.ParseDuration(env("WISHLIST_RELEASE_INTERVAL", "5m"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing WISHLIST_RELEASE_INTERVAL: %w", err)
	}
	cfg.WishlistReleaseInterval = wishlistRelease

	if id := env("MARKETPLACE_WALLET_ID", ""); id != "" {
		shopWallet, err := uuid.Parse(id)
		if err != nil {
			return Config{}, fmt.Errorf("parsing MARKETPLACE_WALLET_ID: %w", err)
		}
		cfg.MarketplaceWalletId = shopWallet
	}

	if id := env("FEE_WALLET_ID", ""); id != "" {
		feeWallet, err := uuid.Parse(id)
		if err != nil {
			return Config{}, fmt.Errorf("parsing FEE_WALLET_ID: %w", err)
		}
		cfg.FeeWalletId = feeWallet
	}

	refundInterval, err := time.ParseDuration(env("CALDRON_REFUND_INTERVAL", "5m"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing CALDRON_REFUND_INTERVAL: %w", err)
	}
	cfg.CaldronRefundInterval = refundInterval

	confirmationTTL, err := time.ParseDuration(env("CONFIRMATION_TTL", "15m"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing CONFIRMATION_TTL: %w", err)
	}
	cfg.ConfirmationTTL = confirmationTTL

	confirmationCooldown, err := time.ParseDuration(env("CONFIRMATION_COOLDOWN", "1m"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing CONFIRMATION_COOLDOWN: %w", err)
	}
	cfg.ConfirmationCooldown = confirmationCooldown

	confirmationLimit, err := strconv.Atoi(env("CONFIRMATION_RATE_LIMIT", "5"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing CONFIRMATION_RATE_LIMIT: %w", err)
	}
	if confirmationLimit < 1 {
		return Config{}, fmt.Errorf("CONFIRMATION_RATE_LIMIT must be positive, got %d", confirmationLimit)
	}
	cfg.ConfirmationRateLimit = confirmationLimit

	confirmationWindow, err := time.ParseDuration(env("CONFIRMATION_RATE_WINDOW", "1h"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing CONFIRMATION_RATE_WINDOW: %w", err)
	}
	cfg.ConfirmationRateWindow = confirmationWindow

	cfg.PublicBaseURL = env("PUBLIC_BASE_URL", "")
	cfg.UsersEndpoint = env("USERS_ENDPOINT", "")
	cfg.SMTPHost = env("SMTP_HOST", "")
	cfg.SMTPUsername = env("SMTP_USERNAME", "")
	cfg.SMTPPassword = env("SMTP_PASSWORD", "")
	cfg.EmailFrom = env("EMAIL_FROM", "")
	cfg.EmailUnsubscribeURL = env("EMAIL_UNSUBSCRIBE_URL", "")
	cfg.EmailSecret = env("EMAIL_SECRET", "")

	smtpPort, err := strconv.Atoi(env("SMTP_PORT", "587"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing SMTP_PORT: %w", err)
	}
	cfg.SMTPPort = smtpPort

	cfg.WebAPIBase = env("WEB_API_BASE", "http://localhost:8080/api/v1")
	cfg.WebClientId = env("WEB_CLIENT_ID", "web")
	cfg.SocialProviders = splitList(env("SOCIAL_PROVIDERS", ""))
	cfg.SocialRedirectBase = env("SOCIAL_REDIRECT_BASE", "")

	connLifetime, err := time.ParseDuration(env("DB_CONN_MAX_LIFETIME", "30m"))
	if err != nil {
		return Config{}, fmt.Errorf("parsing DB_CONN_MAX_LIFETIME: %w", err)
	}
	cfg.DBConnMaxLifetime = connLifetime

	return cfg, nil
}

// splitList разбирает список значений, разделённых запятой.
func splitList(value string) []string {
	if value == "" {
		return nil
	}
	items := strings.Split(value, ",")
	for i, item := range items {
		items[i] = strings.TrimSpace(item)
	}
	return items
}

func env(key, defaultValue string) string {
	v, ok := os.LookupEnv(key)
	if !ok {
		return defaultValue
	}
	return v
}
