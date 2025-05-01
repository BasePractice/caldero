package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"wish/services"
)

func registerHttpHandlers(ctx context.Context, db DatabaseCredit) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /credit", func(w http.ResponseWriter, r *http.Request) {
		createCredit(ctx, db, w, r)
	})
	mux.HandleFunc("GET /credit/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		w.Header().Set("X-Credit-Id", id)
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func createCredit(ctx context.Context, db DatabaseCredit, w http.ResponseWriter, r *http.Request) {
	operator, err := services.HttpAuthorized(r)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodPost:
		var credit CreateCredit
		err := json.NewDecoder(r.Body).Decode(&credit)
		if err != nil {
			slog.Error("Failed decoding credit",
				slog.String("error", err.Error()))
			w.WriteHeader(http.StatusBadRequest)
		} else if !credit.Validate() {
			slog.Error("Credit validation failed",
				slog.String("credit", credit.String()))
			w.WriteHeader(http.StatusBadRequest)
		}
		id, err := db.CreateCredit(ctx, credit, operator)
		if err != nil {
			slog.Error("Failed to create credit",
				slog.String("credit", credit.String()),
				slog.String("error", err.Error()))
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			w.Header().Set("X-Credit-Id", fmt.Sprintf("%d", id))
			w.WriteHeader(http.StatusCreated)
		}
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
