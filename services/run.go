package services

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// shutdownTimeout ограничивает ожидание завершения активных запросов.
const shutdownTimeout = 15 * time.Second

// Run — общая точка входа сервиса: конфигурация, логирование, метрики,
// контекст, отменяемый по сигналу, и код возврата. Раньше эти сорок строк
// были продублированы в четырёх main.go вместе со всеми своими ошибками.
func Run(name string, run func(ctx context.Context, cfg Config) error) {
	cfg, err := LoadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "can't load configuration:", err)
		os.Exit(1)
	}
	if _, err = DefineLogging(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "can't configure logging:", err)
		os.Exit(1)
	}
	DefineMetrics(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logBuildInfo(name)

	stopTracing, err := InitTracing(ctx, name, cfg)
	if err != nil {
		slog.Error("Can't initialize tracing", slog.String("err", err.Error()))
		os.Exit(1)
	}
	defer stopTracing(ctx)

	if err = run(ctx, cfg); err != nil {
		slog.Error("Service stopped with error",
			slog.String("service", name), slog.String("err", err.Error()))
		os.Exit(1)
	}
	slog.Info("Service stopped", slog.String("service", name))
}

// ServeHTTP обслуживает запросы до отмены контекста, затем даёт активным
// запросам завершиться. Раньше сигнал приводил к немедленному os.Exit(0),
// и соединения обрывались на середине.
func ServeHTTP(ctx context.Context, addr string, handler http.Handler) error {
	srv := &http.Server{
		Addr: addr,
		// otelhttp снаружи: спан должен охватывать и восстановление после
		// паники, и измерение, иначе упавший запрос не попадёт в трассу.
		Handler: otelhttp.NewHandler(Recover(handler), "http",
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				if r.Pattern != "" {
					return r.Pattern
				}
				return r.Method + " unmatched"
			})),
		// Без таймаутов сервер по умолчанию держит соединение сколько угодно:
		// открытых, но не отправленных запросов достаточно, чтобы исчерпать
		// ресурсы (Slowloris).
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http server on %s: %w", addr, err)
			return
		}
		errCh <- nil
	}()

	slog.Info("HTTP listening", slog.String("addr", addr))
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("Shutting down HTTP server", slog.String("addr", addr))
		// Контекст намеренно не наследуется: родительский уже отменён
		// сигналом, и производный от него не дал бы запросам завершиться.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		//nolint:contextcheck // см. комментарий выше: отмена родителя — это и есть повод для остановки
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutting down http server on %s: %w", addr, err)
		}
		return nil
	}
}

// ServeGRPC обслуживает gRPC до отмены контекста. По истечении таймаута
// корректной остановки соединения разрываются принудительно, иначе
// зависший стрим не давал бы процессу завершиться.
func ServeGRPC(ctx context.Context, listener net.Listener, srv *grpc.Server) error {
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(listener)
	}()

	slog.Info("gRPC listening", slog.String("addr", listener.Addr().String()))
	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("grpc server on %s: %w", listener.Addr(), err)
		}
		return nil
	case <-ctx.Done():
		slog.Info("Shutting down gRPC server", slog.String("addr", listener.Addr().String()))
		stopped := make(chan struct{})
		go func() {
			srv.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-time.After(shutdownTimeout):
			slog.Warn("Graceful stop timed out, forcing shutdown")
			srv.Stop()
		}
		return nil
	}
}

// RunPeriodic выполняет задачу по расписанию до отмены контекста.
// Горутина завершается вместе с контекстом: горутина без пути выхода —
// это утечка, а не стилистика.
func RunPeriodic(ctx context.Context, name string, interval time.Duration, task func(context.Context) error) {
	if interval <= 0 {
		slog.Info("Periodic task disabled", slog.String("task", name))
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("Periodic task stopped", slog.String("task", name))
			return
		case <-ticker.C:
			if err := task(ctx); err != nil {
				// Сбой одного прогона не повод останавливать расписание.
				slog.Error("Periodic task failed",
					slog.String("task", name), slog.String("err", err.Error()))
			}
		}
	}
}

// Close закрывает ресурс и логирует ошибку: в defer её уже некому вернуть,
// а молчаливое игнорирование скрыло бы, например, незакрытый пул соединений.
func Close(name string, closer io.Closer) {
	if err := closer.Close(); err != nil {
		slog.Error("Can't close resource",
			slog.String("resource", name), slog.String("err", err.Error()))
	}
}

// Recover перехватывает панику обработчика. defer recover() в main для этого
// не годится: обработчики выполняются в других горутинах, и до него паника
// не доходит.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Контекст берётся до вызова обработчика: в отложенной функции
		// обращение к r уже смешивает восстановление после паники
		// с чтением состояния запроса.
		ctx := r.Context()
		defer func() {
			if rec := recover(); rec != nil {
				slog.ErrorContext(ctx, "Recovered from panic in handler",
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.String("panic", fmt.Sprintf("%v", rec)),
					slog.String("stack", string(debug.Stack())))
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// RecoverUnaryInterceptor — то же для gRPC. Сигнатура с any продиктована
// интерфейсом grpc.
func RecoverUnaryInterceptor(
	ctx context.Context,
	req any,
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (resp any, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.ErrorContext(ctx, "Recovered from panic in grpc handler",
				slog.String("method", info.FullMethod),
				slog.String("panic", fmt.Sprintf("%v", rec)),
				slog.String("stack", string(debug.Stack())))
			err = status.Error(codes.Internal, "internal error")
		}
	}()
	return handler(ctx, req)
}
