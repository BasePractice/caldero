package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"wish/services"
	account "wish/services/shared/account"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	accountCreateCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "account_create_counter",
		Help: "Number of Create calls",
	})
)

func registerHttpHandlers(ctx context.Context, db Database) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /account", func(w http.ResponseWriter, r *http.Request) {
		accountCreateCounter.Inc()
		createAccount(ctx, db, w, r)
	})
	prometheus.MustRegister(accountCreateCounter)
	mux.HandleFunc("GET /metrics", promhttp.Handler().ServeHTTP)
	return mux
}

func createAccount(ctx context.Context, db Database, w http.ResponseWriter, r *http.Request) {
	operator, err := services.HttpAuthorized(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var a account.CreateAccount
	if err = json.NewDecoder(r.Body).Decode(&a); err != nil {
		slog.Error("Failed decoding account", slog.String("error", err.Error()))
		http.Error(w, "Malformed request body", http.StatusBadRequest)
		return
	}
	if !a.Validate() {
		slog.Error("Account validation failed", slog.String("account", a.String()))
		http.Error(w, "Account validation failed", http.StatusBadRequest)
		return
	}

	id, err := db.Create(ctx, a, operator)
	if err != nil {
		slog.Error("Failed to create account",
			slog.String("account", a.String()),
			slog.String("error", err.Error()))
		http.Error(w, "Can't create account", http.StatusInternalServerError)
		return
	}
	w.Header().Set("X-Account-Id", fmt.Sprintf("%d", id))
	w.WriteHeader(http.StatusCreated)
}
