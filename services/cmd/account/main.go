package main

import (
	"context"
	"flag"
	"fmt"

	"wish/services"
)

var (
	port = flag.Int("port", 51054, "The service port")
)

func main() {
	flag.Parse()
	services.Run("account", func(ctx context.Context, cfg services.Config) error {
		db, err := NewDatabase(cfg)
		if err != nil {
			return err
		}
		defer services.Close("database", db)

		return services.ServeHTTP(ctx, fmt.Sprintf(":%d", *port),
			registerHttpHandlers(db))
	})
}
