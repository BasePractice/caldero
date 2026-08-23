package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"wish/services"
)

var (
	port        = flag.Int("port", 51054, "The service port")
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
	services.Run("account", func(ctx context.Context, cfg services.Config) error {
		db, err := NewDatabase(ctx, cfg)
		if err != nil {
			return err
		}
		defer services.Close("database", db)

		return services.ServeHTTP(ctx, fmt.Sprintf(":%d", *port),
			registerHttpHandlers(db))
	})
}
