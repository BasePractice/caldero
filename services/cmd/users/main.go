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
	services.Run("users", func(ctx context.Context, cfg services.Config) error {
		service, err := newService(ctx, cfg)
		if err != nil {
			return fmt.Errorf("creating service: %w", err)
		}
		defer services.Close("service", service)

		go services.RunPeriodic(ctx, "token-cleanup", cfg.TokenCleanupInterval,
			service.CleanupExpiredTokens)

		return services.ServeHTTP(ctx, fmt.Sprintf(":%d", *port),
			registerHttpHandlers(service))
	})
}
