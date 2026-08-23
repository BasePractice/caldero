package main

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"wish/services"
	"wish/services/shared/account"

	"github.com/google/uuid"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	accountCreateCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "account_create_counter",
		Help: "Number of Create calls",
	})
)

func registerHttpHandlers(db Database) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /account", func(w http.ResponseWriter, r *http.Request) {
		accountCreateCounter.Inc()
		createAccount(db, w, r)
	})
	mux.HandleFunc("GET /account/{id}", func(w http.ResponseWriter, r *http.Request) {
		getAccount(db, w, r)
	})
	prometheus.MustRegister(accountCreateCounter)
	mux.HandleFunc("GET /metrics", promhttp.Handler().ServeHTTP)
	return mux
}

func createAccount(db Database, w http.ResponseWriter, r *http.Request) {
	operator, err := services.HttpAuthorized(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	a, err := services.DecodeJSON[account.CreateAccount](w, r)
	if err != nil {
		slog.Error("Failed decoding account", slog.String("error", err.Error()))
		services.WriteDecodeError(w, err)
		return
	}
	if !a.Validate() {
		slog.Error("Account validation failed", slog.String("account", a.String()))
		http.Error(w, "Account validation failed", http.StatusBadRequest)
		return
	}
	if !operator.CanActOnBehalfOf(a.UserId) {
		slog.Warn("Attempt to create account for another user without operator role",
			slog.String("operator", operator.Id.String()))
		http.Error(w, "Creating an account for another user requires the operator role",
			http.StatusForbidden)
		return
	}

	id, err := db.Create(r.Context(), a, operator)
	if err != nil {
		slog.Error("Failed to create account",
			slog.String("account", a.String()),
			slog.String("error", err.Error()))
		http.Error(w, "Can't create account", http.StatusInternalServerError)
		return
	}
	w.Header().Set("X-Account-Id", id.String())
	w.WriteHeader(http.StatusCreated)
}

func getAccount(db Database, w http.ResponseWriter, r *http.Request) {
	operator, err := services.HttpAuthorized(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid id", http.StatusBadRequest)
		return
	}

	a, err := db.Get(r.Context(), id)
	if err != nil {
		slog.Error("Get account", slog.String("id", id.String()), slog.String("err", err.Error()))
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	// Идентификатор счёта последовательный, поэтому чужой счёт отдаётся
	// как несуществующий, чтобы не подтверждать его наличие перебором.
	if !operator.CanActOnBehalfOf(a.UserId) {
		slog.Warn("Attempt to read foreign account",
			slog.String("id", id.String()), slog.String("operator", operator.Id.String()))
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err = json.NewEncoder(w).Encode(a); err != nil {
		slog.Error("Encode json", slog.String("id", id.String()), slog.String("err", err.Error()))
	}
}
