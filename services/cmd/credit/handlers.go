package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"wish/services"
	"wish/services/shared/credit"

	"github.com/google/uuid"
)

func registerHttpHandlers(db Database) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /credit", func(w http.ResponseWriter, r *http.Request) {
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
			slog.ErrorContext(r.Context(), "Get credit", slog.String("id", id), slog.String("err", err.Error()))
			http.Error(w, "Can't load credit", http.StatusInternalServerError)
			return
		}
		// Идентификатор кредита последовательный, поэтому чужой кредит
		// отдаётся как несуществующий: 403 подтвердил бы, что он есть,
		// и оставил бы возможность перебора.
		if !operator.CanActOnBehalfOf(c.UserId) && c.CreatorId != operator.Id {
			slog.WarnContext(r.Context(), "Attempt to read foreign credit schedule",
				slog.String("id", id), slog.String("operator", operator.Id.String()))
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		payments, err := monthPaymentCalculation(*c)
		if err != nil {
			slog.ErrorContext(r.Context(), "Payment calculation", slog.String("id", id), slog.String("err", err.Error()))
			http.Error(w, "Can't calculate payment schedule", http.StatusUnprocessableEntity)
			return
		}
		w.Header().Set("X-Credit-Id", id)
		w.Header().Set("Content-Type", "application/json")
		if err = json.NewEncoder(w).Encode(payments); err != nil {
			slog.ErrorContext(r.Context(), "Encode json", slog.String("id", id), slog.String("err", err.Error()))
			return
		}
	})
	// /metrics живёт на служебном порту, а не рядом с публичным API.
	return services.Measure("credit", mux)
}

func createCredit(db Database, w http.ResponseWriter, r *http.Request) {
	operator, err := services.HttpAuthorized(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	c, err := services.DecodeJSON[credit.CreateCredit](w, r)
	if err != nil {
		slog.ErrorContext(r.Context(), "Failed decoding credit", slog.String("error", err.Error()))
		services.WriteDecodeError(w, err)
		return
	}
	if err = c.Validate(); err != nil {
		slog.DebugContext(r.Context(), "Credit validation failed",
			slog.String("credit", c.String()), slog.String("reason", err.Error()))
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Схема заложена под сценарий «оператор выдаёт кредит клиенту»
	// (user_id + creator_id), но роли оператора в системе не было:
	// любой пользователь мог оформить кредит на любого другого.
	if !operator.CanActOnBehalfOf(c.UserId) {
		slog.WarnContext(r.Context(), "Attempt to issue credit to another user without operator role",
			slog.String("operator", operator.Id.String()))
		http.Error(w, "Issuing a credit to another user requires the operator role",
			http.StatusForbidden)
		return
	}

	id, err := db.Create(r.Context(), c, operator)
	if services.IsUniqueViolation(err) {
		http.Error(w, "Such credit already exists", http.StatusConflict)
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "Failed to create credit",
			slog.String("credit", c.String()),
			slog.String("error", err.Error()))
		http.Error(w, "Can't create credit", http.StatusInternalServerError)
		return
	}
	w.Header().Set("X-Credit-Id", id.String())
	w.WriteHeader(http.StatusCreated)
}
