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

	"github.com/joho/godotenv"
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
	services.DefineLogging()
	services.DefineMetrics()
	err := godotenv.Load(".env", ".env.local")
	if err != nil {
		slog.Warn("Warning loading .env file", slog.String("err", err.Error()))
	}
	cdb := NewDatabase()
	handler := registerHttpHandlers(ctx, cdb)
	err = http.ListenAndServe(fmt.Sprintf(":%d", *port), handler)
	if err != nil {
		panic(err)
	}
}
