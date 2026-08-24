package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	wallet "wish/middleware/wallet/v1"
	"wish/services"
)

var (
	port        = flag.Int("port", 51052, "The service port")
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
		services.Migrate("credit", migrations)
		return
	}
	services.Run("credit", func(ctx context.Context, cfg services.Config, health *services.Health) error {
		db, err := NewDatabase(ctx, cfg)
		if err != nil {
			return err
		}
		defer services.Close("database", db)
		health.Register("database", db.Ping)
		services.RegisterDBStats("credit", db)

		var walletClient Wallet
		if cfg.WalletAddress == "" {
			slog.Warn("WALLET_ADDRESS is not set, credit repayment is unavailable")
		} else {
			conn, err := services.NewGrpcClient(cfg.WalletAddress)
			if err != nil {
				return err
			}
			defer services.CloseGrpcClient("wallet", conn)
			walletClient = wallet.NewServiceClient(conn)
		}

		return services.ServeHTTP(ctx, cfg, fmt.Sprintf(":%d", *port),
			registerHttpHandlers(db, walletClient))
	})
}
