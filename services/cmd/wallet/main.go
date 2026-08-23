package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"

	"wish/middleware/wallet"
	"wish/services"

	"google.golang.org/grpc"
)

var (
	port        = flag.Int("port", 51051, "The service port")
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
	services.Run("wallet", func(ctx context.Context, cfg services.Config) error {
		listen, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
		if err != nil {
			return fmt.Errorf("listening on port %d: %w", *port, err)
		}

		cache, err := services.NewDefaultCache(ctx, cfg)
		if err != nil {
			return fmt.Errorf("connecting to cache: %w", err)
		}
		defer services.Close("cache", cache)

		db, err := NewDatabaseWallet(ctx, cfg)
		if err != nil {
			return err
		}
		defer services.Close("database", db)

		grpcServer := grpc.NewServer(
			grpc.UnaryInterceptor(services.RecoverUnaryInterceptor),
			grpc.MaxRecvMsgSize(services.MaxRequestBody),
		)
		wallet.RegisterServiceServer(grpcServer, &service{db: db, cache: cache})
		return services.ServeGRPC(ctx, listen, grpcServer)
	})
}
