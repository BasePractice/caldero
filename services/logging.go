package services

import (
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/arl/statsviz"
	"github.com/lmittmann/tint"
	"github.com/mattn/go-colorable"
)

func DefineLogging() *slog.Logger {
	var level *slog.Level
	debugLevel := slog.LevelDebug
	level = &debugLevel
	_ = level.UnmarshalText([]byte(LogLevel))
	opts := &slog.HandlerOptions{Level: level, AddSource: true}
	var handler slog.Handler = slog.NewTextHandler(os.Stdout, opts)
	if LogFile != "" {
		file, err := os.OpenFile(LogFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0666)
		if err != nil {
			handler = slog.NewJSONHandler(file, opts)
		} else {
			log.Fatal(err)
		}
	} else if LogColor != "" {
		handler = tint.NewHandler(colorable.NewColorable(os.Stdout), &tint.Options{
			Level:      level,
			TimeFormat: time.DateTime,
			AddSource:  true,
		})
	}
	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}

func DefineMetrics() {
	i, err := strconv.Atoi(MetricsPort)
	if err != nil {
		slog.Error("Error parsing metrics port",
			slog.String("port", MetricsPort), slog.String("err", err.Error()))
		i = 8081
	}
	mPort := flag.Int("mport", i, "Metrics port")
	mux := http.NewServeMux()
	err = statsviz.Register(mux)
	if err != nil {
		slog.Error("Error registering metrics", slog.String("err", err.Error()))
	} else {
		go func() {
			slog.Info("Metrics listening on port", slog.String("port", strconv.Itoa(*mPort)))
			_ = http.ListenAndServe(":"+strconv.Itoa(*mPort), mux)
		}()
	}
}
