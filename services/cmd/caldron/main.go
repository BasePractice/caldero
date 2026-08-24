package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"wish/services"
	"wish/services/shared/marketplace/cached"
	"wish/services/shared/notify"
	"wish/services/shared/wallets"
)

var (
	port        = flag.Int("port", 51057, "The service port")
	healthcheck = flag.Bool("healthcheck", false, "Check that the service accepts connections and exit")
	// migrate — шаг доставки: миграции применяются до подмены образов,
	// а не при старте сервиса.
	migrate = flag.Bool("migrate", false, "Apply database migrations and exit")
)

func main() {
	flag.Parse()
	if *healthcheck {
		if err := services.Healthcheck(fmt.Sprintf("localhost:%d", *port)); err != nil {
			fmt.Fprintln(os.Stderr, "healthcheck failed:", err)
			os.Exit(1)
		}
		return
	}
	if *migrate {
		services.Migrate("caldron", migrations)
		return
	}
	services.Run("caldron", func(ctx context.Context, cfg services.Config, health *services.Health) error {
		db, err := NewDatabase(ctx, cfg)
		if err != nil {
			return err
		}
		defer services.Close("database", db)
		health.Register("database", db.Ping)
		services.RegisterDBStats("caldron", db)

		var wallet Wallet
		if cfg.WalletAddress != "" {
			conn, err := services.NewGrpcClient(cfg.WalletAddress)
			if err != nil {
				return fmt.Errorf("connecting to wallet: %w", err)
			}
			defer services.CloseGrpcClient("wallet", conn)
			wallet = wallets.NewClient(conn, cfg.ServiceUserId)
		} else {
			// Котёл без кошелька собрать нельзя: взносы — это деньги,
			// а не отметка в таблице. Сервис поднимается, но взносы
			// будут отказывать, и об этом лучше знать сразу.
			slog.Warn("Wallet is not configured: contributions will fail")
		}

		notifier := notify.NewClient(cfg.NotifyEndpoint, cfg.ServiceUserId)
		if !notifier.Enabled() {
			slog.Warn("Notifications are disabled: NOTIFY_ENDPOINT or SERVICE_USER_ID is empty")
		}

		cache, err := services.NewDefaultCache(ctx, cfg)
		if err != nil {
			return fmt.Errorf("connecting to cache: %w", err)
		}
		defer services.Close("cache", cache)

		catalogs, err := cached.Build(cfg.MarketplaceProviders, cache, cfg.MarketplaceCacheTTL)
		if err != nil {
			return err
		}

		caldrons := NewCaldrons(db, wallet, notifier, catalogs)
		// Возвраты добиваются фоном: сбой посреди отмены иначе оставил бы
		// средства участников в котле навсегда.
		go services.RunPeriodic(ctx, "caldron-refunds", cfg.CaldronRefundInterval,
			caldrons.RefundPending)

		return services.ServeHTTP(ctx, cfg, fmt.Sprintf(":%d", *port), registerHttpHandlers(caldrons))
	})
}
