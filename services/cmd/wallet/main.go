package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"

	wallet "wish/middleware/wallet/v1"
	"wish/services"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	grpchealth "google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
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
	services.Run("wallet", func(ctx context.Context, cfg services.Config, health *services.Health) error {
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
		health.Register("database", db.Ping)
		services.RegisterDBStats("wallet", db)

		// Партиции создаются заранее: когда окно кончится, все транзакции
		// пойдут в partition default и партиционирование потеряет смысл.
		if created, err := db.EnsurePartitions(ctx, cfg.PartitionMonthsAhead); err != nil {
			return fmt.Errorf("preparing transaction partitions: %w", err)
		} else if created > 0 {
			slog.Info("Transaction partitions created", slog.Int("count", created))
		}
		go services.RunPeriodic(ctx, "partition-maintenance", cfg.PartitionMaintenanceInterval,
			func(ctx context.Context) error {
				created, err := db.EnsurePartitions(ctx, cfg.PartitionMonthsAhead)
				if err != nil {
					return err
				}
				if created > 0 {
					slog.Info("Transaction partitions created", slog.Int("count", created))
				}
				return nil
			})
		services.RegisterDefaultPartitionRows("wallet", func() int64 {
			rows, err := db.DefaultPartitionRows(ctx)
			if err != nil {
				slog.Error("Can't count default partition rows", slog.String("err", err.Error()))
				return -1
			}
			return rows
		})

		grpcServer := grpc.NewServer(
			// Обработчик статистики otelgrpc: интерсепторы для трассировки
			// в новых версиях помечены устаревшими.
			grpc.StatsHandler(otelgrpc.NewServerHandler()),
			grpc.ChainUnaryInterceptor(
				services.MeasureUnaryInterceptor("wallet"),
				services.RecoverUnaryInterceptor,
				// Служебные методы проверки доступны без пользователя:
				// оркестратор опрашивает их не от чьего-либо имени.
				services.AuthorizeUnaryInterceptor(
					"/grpc.health.v1.Health/Check",
					"/grpc.health.v1.Health/Watch",
				),
			),
			grpc.MaxRecvMsgSize(services.MaxRequestBody),
		)
		wallet.RegisterServiceServer(grpcServer, &service{db: db, cache: cache})

		// Стандартная проверка здоровья: у gRPC нет аналога HTTP-эндпоинта,
		// и без неё оркестратору нечем опросить сервис.
		healthServer := grpchealth.NewServer()
		healthServer.SetServingStatus("wallet.v1.Service", healthpb.HealthCheckResponse_SERVING)
		healthpb.RegisterHealthServer(grpcServer, healthServer)

		// Reflection нужен инструментам отладки: без него клиент обязан
		// иметь при себе .proto-файл.
		reflection.Register(grpcServer)
		return services.ServeGRPC(ctx, listen, grpcServer)
	})
}
