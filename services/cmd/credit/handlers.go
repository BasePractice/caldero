package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"wish/services"
	"wish/services/shared/credit"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	creditCreateCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "credit_create_counter",
		Help: "Number of Create calls",
	})
)

func registerHttpHandlers(ctx context.Context, db Database) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /credit", func(w http.ResponseWriter, r *http.Request) {
		creditCreateCounter.Inc()
		createCredit(ctx, db, w, r)
	})
	mux.HandleFunc("GET /credits/{id}/schedule", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		idInt, err := strconv.ParseUint(id, 10, 64)
		if err != nil {
			slog.Error("Invalid id", slog.String("id", id), slog.String("err", err.Error()))
			http.Error(w, "Invalid id", http.StatusBadRequest)
			return
		}
		c, err := db.Get(ctx, idInt)
		if err != nil {
			slog.Error("Get credit", slog.String("id", id), slog.String("err", err.Error()))
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		payments := mothPaymentCalculation(*c)
		w.Header().Set("X-Credit-Id", id)
		w.Header().Set("Content-Type", "application/json")
		err = json.NewEncoder(w).Encode(payments)
		if err != nil {
			slog.Error("Encode json", slog.String("id", id), slog.String("err", err.Error()))
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	})
	prometheus.MustRegister(creditCreateCounter)
	mux.HandleFunc("GET /metrics", promhttp.Handler().ServeHTTP)
	return mux
}

func createCredit(ctx context.Context, db Database, w http.ResponseWriter, r *http.Request) {
	operator, err := services.HttpAuthorized(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var c credit.CreateCredit
	if err = json.NewDecoder(r.Body).Decode(&c); err != nil {
		slog.Error("Failed decoding credit", slog.String("error", err.Error()))
		http.Error(w, "Malformed request body", http.StatusBadRequest)
		return
	}
	if !c.Validate() {
		slog.Error("Credit validation failed", slog.String("credit", c.String()))
		http.Error(w, "Credit validation failed", http.StatusBadRequest)
		return
	}

	id, err := db.Create(ctx, c, operator)
	if err != nil {
		slog.Error("Failed to create credit",
			slog.String("credit", c.String()),
			slog.String("error", err.Error()))
		http.Error(w, "Can't create credit", http.StatusInternalServerError)
		return
	}
	w.Header().Set("X-Credit-Id", fmt.Sprintf("%d", id))
	w.WriteHeader(http.StatusCreated)
}
