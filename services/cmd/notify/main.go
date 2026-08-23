package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"wish/services"
)

var (
	port        = flag.Int("port", 51055, "The service port")
	healthcheck = flag.Bool("healthcheck", false, "Check that the service accepts connections and exit")
)

// queueDepthTimeout ограничивает запрос длины очереди: метрику собирает
// Prometheus, и застрявший запрос задержал бы весь сбор.
const queueDepthTimeout = 2 * time.Second

func main() {
	flag.Parse()
	if *healthcheck {
		if err := services.Healthcheck(fmt.Sprintf("localhost:%d", *port)); err != nil {
			fmt.Fprintln(os.Stderr, "healthcheck failed:", err)
			os.Exit(1)
		}
		return
	}
	services.Run("notify", func(ctx context.Context, cfg services.Config, health *services.Health) error {
		db, err := NewDatabase(ctx, cfg)
		if err != nil {
			return err
		}
		defer services.Close("database", db)
		health.Register("database", db.Ping)
		services.RegisterDBStats("notify", db)
		registerQueueDepth(func() int64 {
			depthCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), queueDepthTimeout)
			defer cancel()
			pending, err := db.Unsettled(depthCtx)
			if err != nil {
				slog.WarnContext(ctx, "Can't measure queue depth", slog.String("err", err.Error()))
				return -1
			}
			return int64(pending)
		})

		// Шаблоны проверяются при старте: пропущенный текст иначе
		// обнаружится в момент, когда сообщение уже нужно доставить.
		templates, err := LoadTemplates()
		if err != nil {
			return err
		}

		hub := NewHub()
		bus, err := NewBus(ctx, cfg, hub)
		if err != nil {
			return fmt.Errorf("connecting notification bus: %w", err)
		}
		defer services.Close("bus", bus)

		senders := []Sender{NewInApp(db, bus)}

		// Боты: Telegram описан значениями по умолчанию, остальные
		// площадки задаются конфигурацией — выдумывать чужой протокол
		// нельзя, а механизм у них общий.
		messengers, err := LoadMessengers(db, cfg.NotifyTelegramToken,
			cfg.NotifyTelegramAPI, cfg.NotifyTelegramBot)
		if err != nil {
			return err
		}
		for _, messenger := range messengers {
			senders = append(senders, messenger)
			background(ctx, string(messenger.Channel())+"-updates", messenger.Run)
		}
		if len(messengers) == 0 {
			// Не ошибка: локальный стенд поднимается без ботов,
			// и лента приложения работает сама по себе.
			slog.Warn("No messengers are configured")
		}

		var email *Email
		emailConfig := EmailConfig{
			Host: cfg.SMTPHost, Port: cfg.SMTPPort,
			Username: cfg.SMTPUsername, Password: cfg.SMTPPassword,
			From: cfg.EmailFrom, UnsubscribeBase: cfg.EmailUnsubscribeURL,
			Secret: cfg.EmailSecret,
		}
		switch {
		case emailConfig.Enabled():
			email = NewEmail(NewUsersContacts(cfg.UsersEndpoint, cfg.ServiceUserId), emailConfig)
			senders = append(senders, email)
		case cfg.SMTPHost != "":
			// Настроенный наполовину канал молча не отправляет ничего —
			// об этом лучше сказать при старте.
			return emailConfig.Validate()
		default:
			slog.Warn("Email channel is disabled: SMTP is not configured")
		}

		dispatcher := NewDispatcher(db, templates, senders...)
		dispatcher.RateLimit = cfg.NotifyRateLimit
		dispatcher.RateWindow = cfg.NotifyRateWindow

		background(ctx, "bus", bus.Run)
		background(ctx, "dispatcher", dispatcher.Run)

		return services.ServeHTTP(ctx, cfg, fmt.Sprintf(":%d", *port), registerHttpHandlers(&api{
			db:         db,
			hub:        hub,
			messengers: messengers,
			email:      email,
			codeTTL:    cfg.NotifyBindingCodeTTL,
			wsOrigins:  cfg.NotifyWebSocketOrigins,
		}))
	})
}

// background запускает фоновую задачу. Каждая завершается по контексту:
// горутина без пути выхода — это утечка, а не стилистика.
func background(ctx context.Context, name string, run func(context.Context) error) {
	go func() {
		if err := run(ctx); err != nil {
			slog.ErrorContext(ctx, "Background task stopped",
				slog.String("task", name), slog.String("err", err.Error()))
		}
	}()
}
