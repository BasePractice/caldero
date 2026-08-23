package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"wish/services"
)

var (
	port        = flag.Int("port", 51053, "The service port")
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
	services.Run("users", func(ctx context.Context, cfg services.Config, health *services.Health) error {
		service, err := newService(ctx, cfg)
		if err != nil {
			return fmt.Errorf("creating service: %w", err)
		}
		defer services.Close("service", service)
		health.Register("database", service.Ping)
		services.RegisterDBStats("users", service.Stats())

		go services.RunPeriodic(ctx, "token-cleanup", cfg.TokenCleanupInterval,
			service.CleanupExpiredTokens)
		// Брошенные состояния внешнего входа накапливаются сами собой:
		// пользователь начал вход и закрыл вкладку.
		go services.RunPeriodic(ctx, "social-login-cleanup", cfg.TokenCleanupInterval,
			service.CleanupSocialLogins)

		return services.ServeHTTP(ctx, cfg, fmt.Sprintf(":%d", *port),
			registerHttpHandlers(service))
	})
}
