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
		var telegram *Telegram
		if cfg.NotifyTelegramToken != "" {
			telegram = NewTelegram(db, cfg.NotifyTelegramToken, cfg.NotifyTelegramAPI)
			senders = append(senders, telegram)
		} else {
			// Не ошибка: локальный стенд поднимается без бота, и лента
			// приложения работает сама по себе.
			slog.Warn("Telegram channel is disabled: NOTIFY_TELEGRAM_TOKEN is empty")
		}

		dispatcher := NewDispatcher(db, templates, senders...)
		dispatcher.RateLimit = cfg.NotifyRateLimit
		dispatcher.RateWindow = cfg.NotifyRateWindow

		background(ctx, "bus", bus.Run)
		background(ctx, "dispatcher", dispatcher.Run)
		if telegram != nil {
			background(ctx, "telegram-updates", telegram.Run)
		}

		return services.ServeHTTP(ctx, cfg, fmt.Sprintf(":%d", *port), registerHttpHandlers(&api{
			db:        db,
			hub:       hub,
			telegram:  telegram,
			botName:   cfg.NotifyTelegramBot,
			codeTTL:   cfg.NotifyBindingCodeTTL,
			wsOrigins: cfg.NotifyWebSocketOrigins,
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
