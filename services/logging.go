package services

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/arl/statsviz"
	"github.com/lmittmann/tint"
	"github.com/mattn/go-colorable"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

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
		handler = slog.NewJSONHandler(file, opts)
	case cfg.LogColor:
		handler = tint.NewHandler(colorable.NewColorable(os.Stdout), &tint.Options{
			Level:      level,
			TimeFormat: time.DateTime,
			AddSource:  true,
		})
	default:
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger, nil
}

// DefineMetrics поднимает служебный порт с /metrics. Порт отдельный от
// публичного: метрики не должны быть доступны снаружи вместе с API.
// statsviz подключается только при отладке — он публикует внутреннее
// состояние рантайма без какой-либо аутентификации.
func DefineMetrics(cfg Config) {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.Handler())
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
