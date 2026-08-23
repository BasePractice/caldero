package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"

	"wish/services"
)

var (
	port = flag.Int("port", 51054, "The service port")
)

func main() {
	defer func() {
		if err := recover(); err != nil {
			slog.Error("Recovered from panic",
				slog.String("stack", string(debug.Stack())),
				slog.String("err", fmt.Sprintf("%v", err)))
		}
	}()
	ctx := services.ExitHandle(func(context.Context) {
		slog.Info("Service exit")
		os.Exit(0)
	})
	flag.Parse()
	cfg, err := services.LoadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "can't load configuration:", err)
		os.Exit(1)
	}
	if _, err = services.DefineLogging(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "can't configure logging:", err)
		os.Exit(1)
	}
	services.DefineMetrics(cfg)
	cdb := NewDatabase(cfg)
	handler := registerHttpHandlers(ctx, cdb)
	err = http.ListenAndServe(fmt.Sprintf(":%d", *port), handler)
	if err != nil {
		panic(err)
	}
}
