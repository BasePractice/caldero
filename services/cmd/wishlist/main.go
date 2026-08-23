package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"wish/services"
	"wish/services/shared/credit"
	"wish/services/shared/marketplace"
	"wish/services/shared/notify"
	"wish/services/shared/payment"

	"github.com/google/uuid"
)

var (
	port        = flag.Int("port", 51056, "The service port")
	healthcheck = flag.Bool("healthcheck", false, "Check that the service accepts connections and exit")
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
	services.Run("wishlist", func(ctx context.Context, cfg services.Config, health *services.Health) error {
		db, err := NewDatabase(ctx, cfg)
		if err != nil {
			return err
		}
		defer services.Close("database", db)
		health.Register("database", db.Ping)
		services.RegisterDBStats("wishlist", db)

		cache, err := services.NewDefaultCache(ctx, cfg)
		if err != nil {
			return fmt.Errorf("connecting to cache: %w", err)
		}
		defer services.Close("cache", cache)

		catalogs, err := buildCatalogs(cfg, cache)
		if err != nil {
			return err
		}

		fee := payment.Fee{
			BasisPoints: cfg.PaymentFeeBasisPoints,
			Fixed:       credit.Amount(cfg.PaymentFeeFixed),
			Min:         credit.Amount(cfg.PaymentFeeMin),
			Max:         credit.Amount(cfg.PaymentFeeMax),
		}
		if err = fee.Validate(); err != nil {
			return fmt.Errorf("invalid fee configuration: %w", err)
		}

		var wallet Wallet
		if cfg.WalletAddress != "" {
			conn, err := services.NewGrpcClient(cfg.WalletAddress)
			if err != nil {
				return fmt.Errorf("connecting to wallet: %w", err)
			}
			defer services.CloseGrpcClient("wallet", conn)
			wallet = NewWalletClient(conn, cfg.ServiceUserId)
		} else {
			// Не ошибка старта: товарная часть списка работает и без
			// кошелька, отказ придёт только на денежном подарке.
			slog.Warn("Wallet is not configured: money gifts are disabled")
		}

		notifier := notify.NewClient(cfg.NotifyEndpoint, cfg.ServiceUserId)
		if !notifier.Enabled() {
			slog.Warn("Notifications are disabled: NOTIFY_ENDPOINT or SERVICE_USER_ID is empty")
		}

		var feeWallet *uuid.UUID
		if cfg.FeeWalletId != uuid.Nil {
			feeWallet = &cfg.FeeWalletId
		} else if fee.For(credit.Amount(1_000_00)) > 0 {
			// Тариф задан, а удерживать некуда: молча терять комиссию
			// хуже, чем сказать об этом при старте.
			slog.Warn("Fee is configured but FEE_WALLET_ID is empty: fee will not be charged")
		}

		gifts := NewGifts(db, catalogs, notifier, wallet, fee, feeWallet, cfg.WishlistReservationTTL)
		go services.RunPeriodic(ctx, "reservation-release", cfg.WishlistReleaseInterval,
			gifts.ReleaseExpired)

		return services.ServeHTTP(ctx, cfg, fmt.Sprintf(":%d", *port), registerHttpHandlers(gifts))
	})
}

// buildCatalogs собирает реестр площадок. Каждая оборачивается кэшем:
// у публичных API есть ограничения частоты, и обращаться к площадке
// на каждый показ списка нельзя.
func buildCatalogs(cfg services.Config, cache services.Cache) (*marketplace.Registry, error) {
	catalogs := make([]marketplace.Catalog, 0, len(cfg.MarketplaceProviders))
	for _, provider := range cfg.MarketplaceProviders {
		switch marketplace.Provider(provider) {
		case marketplace.ProviderStub:
			catalogs = append(catalogs,
				marketplace.NewCached(&marketplace.Stub{}, cache, cfg.MarketplaceCacheTTL))
		case marketplace.ProviderOzon, marketplace.ProviderWildberry:
			// Адаптеры площадок — T-076: их нельзя написать, не выяснив,
			// что доступно стороннему приложению (ADR 0004).
			return nil, fmt.Errorf("marketplace %s is not implemented yet", provider)
		default:
			return nil, fmt.Errorf("unknown marketplace %q", provider)
		}
	}
	return marketplace.NewRegistry(catalogs...), nil
}
