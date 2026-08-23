package main

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"wish/services"
	"wish/services/shared/account"

	"github.com/google/uuid"
)

func registerHttpHandlers(db Database) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /account", func(w http.ResponseWriter, r *http.Request) {
		createAccount(db, w, r)
	})
	mux.HandleFunc("GET /account/{id}", func(w http.ResponseWriter, r *http.Request) {
		getAccount(db, w, r)
	})
	return services.Measure("account", mux)
}

func createAccount(db Database, w http.ResponseWriter, r *http.Request) {
	operator, err := services.HttpAuthorized(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	a, err := services.DecodeJSON[account.CreateAccount](w, r)
	if err != nil {
		slog.ErrorContext(r.Context(), "Failed decoding account", slog.String("error", err.Error()))
		services.WriteDecodeError(w, err)
		return
	}
	if err = a.Validate(); err != nil {
		slog.DebugContext(r.Context(), "Account validation failed",
			slog.String("account", a.String()), slog.String("reason", err.Error()))
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !operator.CanActOnBehalfOf(a.UserId) {
		slog.WarnContext(r.Context(), "Attempt to create account for another user without operator role",
			slog.String("operator", operator.Id.String()))
		http.Error(w, "Creating an account for another user requires the operator role",
			http.StatusForbidden)
		return
	}

	id, err := db.Create(r.Context(), a, operator)
	if services.IsUniqueViolation(err) {
		http.Error(w, "Such account already exists", http.StatusConflict)
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "Failed to create account",
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
		slog.ErrorContext(r.Context(), "Get account", slog.String("id", id.String()), slog.String("err", err.Error()))
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	// Идентификатор счёта последовательный, поэтому чужой счёт отдаётся
	// как несуществующий, чтобы не подтверждать его наличие перебором.
	if !operator.CanActOnBehalfOf(a.UserId) {
		slog.WarnContext(r.Context(), "Attempt to read foreign account",
			slog.String("id", id.String()), slog.String("operator", operator.Id.String()))
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err = json.NewEncoder(w).Encode(a); err != nil {
		slog.ErrorContext(r.Context(), "Encode json", slog.String("id", id.String()), slog.String("err", err.Error()))
	}
}
