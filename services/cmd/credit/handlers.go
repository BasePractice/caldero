package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"wish/services"
	"wish/services/shared/credit"

	"github.com/google/uuid"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	creditCreateCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "credit_create_counter",
		Help: "Number of Create calls",
	})
)

func registerHttpHandlers(db Database) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /credit", func(w http.ResponseWriter, r *http.Request) {
		creditCreateCounter.Inc()
		createCredit(db, w, r)
	})
	mux.HandleFunc("GET /credits/{id}/schedule", func(w http.ResponseWriter, r *http.Request) {
		operator, err := services.HttpAuthorized(r)
		if err != nil {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		id := r.PathValue("id")
		creditId, err := uuid.Parse(id)
		if err != nil {
			http.Error(w, "Invalid id", http.StatusBadRequest)
			return
		}
		c, err := db.Get(r.Context(), creditId)
		if errors.Is(err, ErrCreditNotFound) {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		if err != nil {
			slog.Error("Get credit", slog.String("id", id), slog.String("err", err.Error()))
			http.Error(w, "Can't load credit", http.StatusInternalServerError)
			return
		}
		// Идентификатор кредита последовательный, поэтому чужой кредит
		// отдаётся как несуществующий: 403 подтвердил бы, что он есть,
		// и оставил бы возможность перебора.
		if c.UserId != operator.Id && c.CreatorId != operator.Id {
			slog.Warn("Attempt to read foreign credit schedule",
				slog.String("id", id), slog.String("operator", operator.Id.String()))
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		payments, err := monthPaymentCalculation(*c)
		if err != nil {
			slog.Error("Payment calculation", slog.String("id", id), slog.String("err", err.Error()))
			http.Error(w, "Can't calculate payment schedule", http.StatusUnprocessableEntity)
			return
		}
		w.Header().Set("X-Credit-Id", id)
		w.Header().Set("Content-Type", "application/json")
		if err = json.NewEncoder(w).Encode(payments); err != nil {
			slog.Error("Encode json", slog.String("id", id), slog.String("err", err.Error()))
			return
		}
	})
	prometheus.MustRegister(creditCreateCounter)
	mux.HandleFunc("GET /metrics", promhttp.Handler().ServeHTTP)
	return mux
}

func createCredit(db Database, w http.ResponseWriter, r *http.Request) {
	operator, err := services.HttpAuthorized(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	c, err := services.DecodeJSON[credit.CreateCredit](w, r)
	if err != nil {
		slog.Error("Failed decoding credit", slog.String("error", err.Error()))
		services.WriteDecodeError(w, err)
		return
	}
	if !c.Validate() {
		slog.Error("Credit validation failed", slog.String("credit", c.String()))
		http.Error(w, "Credit validation failed", http.StatusBadRequest)
		return
	}

	id, err := db.Create(r.Context(), c, operator)
	if err != nil {
		slog.Error("Failed to create credit",
			slog.String("credit", c.String()),
			slog.String("error", err.Error()))
		http.Error(w, "Can't create credit", http.StatusInternalServerError)
		return
	}
	w.Header().Set("X-Credit-Id", id.String())
	w.WriteHeader(http.StatusCreated)
}
