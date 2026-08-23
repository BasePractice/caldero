package services

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/arl/statsviz"
	"github.com/lmittmann/tint"
	"github.com/mattn/go-colorable"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/trace"
)

// traceHandler добавляет к записи идентификаторы трассы. Без них лог и трасса
// живут отдельно, и по записи в Kibana нельзя перейти к тому, что происходило
// в этом же запросе.
type traceHandler struct {
	slog.Handler
}

func (h traceHandler) Handle(ctx context.Context, record slog.Record) error {
	if span := trace.SpanContextFromContext(ctx); span.IsValid() {
		record.AddAttrs(
			slog.String("trace_id", span.TraceID().String()),
			slog.String("span_id", span.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, record)
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{h.Handler.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{h.Handler.WithGroup(name)}
}

func DefineLogging(cfg Config) (*slog.Logger, error) {
	level := slog.LevelDebug
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return nil, fmt.Errorf("parsing log level %q: %w", cfg.LogLevel, err)
	}
	opts := &slog.HandlerOptions{Level: level, AddSource: true}

	var handler slog.Handler
	switch {
	case cfg.LogFile != "":
		file, err := os.OpenFile(cfg.LogFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return nil, fmt.Errorf("opening log file %q: %w", cfg.LogFile, err)
		}
		// Запись идёт и в файл, и в stdout: файл читает сборщик логов,
		// а stdout нужен, чтобы docker compose logs продолжал работать.
		handler = slog.NewJSONHandler(io.MultiWriter(os.Stdout, file), opts)
	case cfg.LogColor:
		handler = tint.NewHandler(colorable.NewColorable(os.Stdout), &tint.Options{
			Level:      level,
			TimeFormat: time.DateTime,
			AddSource:  true,
		})
	default:
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	logger := slog.New(traceHandler{handler})
	slog.SetDefault(logger)
	return logger, nil
}

// DefineMetrics поднимает служебный порт с /metrics. Порт отдельный от
// публичного: метрики не должны быть доступны снаружи вместе с API.
// statsviz подключается только при отладке — он публикует внутреннее
// состояние рантайма без какой-либо аутентификации.
func DefineMetrics(cfg Config, health *Health) {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.Handler())
	if health != nil {
		mux.Handle("/", health.Handler())
	}
	if cfg.DebugStatsviz {
		if err := statsviz.Register(mux); err != nil {
			slog.Error("Error registering statsviz", slog.String("err", err.Error()))
		}
	}

	addr := ":" + strconv.Itoa(cfg.MetricsPort)
	// Явный сервер, а не http.ListenAndServe: тот не даёт задать таймауты,
	// и служебный порт остаётся уязвим к медленным соединениям.
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		slog.Info("Metrics listening", slog.String("addr", addr))
		if err := srv.ListenAndServe(); err != nil {
			slog.Error("Metrics server stopped",
				slog.String("addr", addr), slog.String("err", err.Error()))
		}
	}()
}
