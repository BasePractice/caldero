package main

import (
	"context"
	"flag"
	"fmt"

	"wish/services"
)

var (
	port = flag.Int("port", 51053, "The service port")
)

func main() {
	flag.Parse()
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
