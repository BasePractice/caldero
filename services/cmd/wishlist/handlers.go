package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"wish/services"
	"wish/services/shared/wishlist"

	"github.com/google/uuid"
)

type api struct {
	gifts *Gifts
}

func registerHttpHandlers(gifts *Gifts) http.Handler {
	a := &api{gifts: gifts}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /wishlist/items", a.add)
	mux.HandleFunc("GET /wishlist/items", a.list)
	mux.HandleFunc("GET /wishlist/chosen", a.chosen)
	mux.HandleFunc("GET /wishlist/{user}/items", a.foreign)
	mux.HandleFunc("DELETE /wishlist/items/{id}", a.remove)
	mux.HandleFunc("POST /wishlist/items/{id}/hide", a.act(actionHide))
	mux.HandleFunc("POST /wishlist/items/{id}/show", a.act(actionShow))
	mux.HandleFunc("POST /wishlist/items/{id}/reserve", a.act(actionReserve))
	mux.HandleFunc("POST /wishlist/items/{id}/cancel", a.act(actionCancel))
	mux.HandleFunc("POST /wishlist/items/{id}/confirm", a.act(actionConfirm))
	mux.HandleFunc("POST /wishlist/items/{id}/reject", a.act(actionReject))
	mux.HandleFunc("POST /wishlist/items/{id}/accept", a.act(actionAccept))
	return services.Measure("wishlist", mux)
}

func (a *api) add(w http.ResponseWriter, r *http.Request) {
	authorized, err := services.HttpAuthorized(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	create, err := services.DecodeJSON[wishlist.CreateItem](w, r)
	if err != nil {
		services.WriteDecodeError(w, err)
		return
	}
	if err = create.Validate(); err != nil {
		slog.DebugContext(r.Context(), "Item validation failed",
			slog.String("item", create.String()), slog.String("reason", err.Error()))
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	item, err := a.gifts.Add(r.Context(), authorized.Id, create)
	if err != nil {
		writeError(r.Context(), w, "Can't add item", err)
		return
	}
	w.Header().Set("X-Item-Id", item.Id.String())
	writeJSON(r.Context(), w, http.StatusCreated, item)
}

func (a *api) list(w http.ResponseWriter, r *http.Request) {
	authorized, err := services.HttpAuthorized(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	items, err := a.gifts.List(r.Context(), authorized.Id, authorized.Id)
	if err != nil {
		writeError(r.Context(), w, "Can't load wishlist", err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, items)
}

// foreign отдаёт чужой список: только то, что владелец готов принять
// в подарок.
func (a *api) foreign(w http.ResponseWriter, r *http.Request) {
	authorized, err := services.HttpAuthorized(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	owner, err := uuid.Parse(r.PathValue("user"))
	if err != nil {
		http.Error(w, "Invalid user id", http.StatusBadRequest)
		return
	}

	items, err := a.gifts.List(r.Context(), authorized.Id, owner)
	if err != nil {
		writeError(r.Context(), w, "Can't load wishlist", err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, items)
}

func (a *api) chosen(w http.ResponseWriter, r *http.Request) {
	authorized, err := services.HttpAuthorized(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	items, err := a.gifts.Chosen(r.Context(), authorized.Id)
	if err != nil {
		writeError(r.Context(), w, "Can't load chosen gifts", err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, items)
}

func (a *api) remove(w http.ResponseWriter, r *http.Request) {
	authorized, err := services.HttpAuthorized(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid id", http.StatusBadRequest)
		return
	}

	if err = a.gifts.db.Delete(r.Context(), id, authorized.Id); err != nil {
		writeError(r.Context(), w, "Can't delete item", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// action — операция над элементом. Обработчики у них одинаковы во всём,
// кроме одного вызова, поэтому различие вынесено в тип, а не размножено
// по семи почти одинаковым функциям.
type action string

const (
	actionHide    action = "hide"
	actionShow    action = "show"
	actionReserve action = "reserve"
	actionCancel  action = "cancel"
	actionConfirm action = "confirm"
	actionReject  action = "reject"
	actionAccept  action = "accept"
)

func (a *api) act(what action) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authorized, err := services.HttpAuthorized(r)
		if err != nil {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			http.Error(w, "Invalid id", http.StatusBadRequest)
			return
		}

		var item wishlist.Item
		switch what {
		case actionHide:
			item, err = a.gifts.Hide(r.Context(), authorized.Id, id)
		case actionShow:
			item, err = a.gifts.Show(r.Context(), authorized.Id, id)
		case actionReserve:
			item, err = a.gifts.Reserve(r.Context(), authorized.Id, id)
		case actionCancel:
			item, err = a.gifts.Cancel(r.Context(), authorized.Id, id)
		case actionConfirm:
			item, err = a.gifts.Confirm(r.Context(), authorized.Id, id)
		case actionReject:
			item, err = a.gifts.Reject(r.Context(), authorized.Id, id)
		case actionAccept:
			item, err = a.gifts.Accept(r.Context(), authorized.Id, id)
		}
		if err != nil {
			writeError(r.Context(), w, "Can't "+string(what)+" item", err)
			return
		}
		writeJSON(r.Context(), w, http.StatusOK, item.Public(authorized.Id))
	}
}

// writeError переводит доменную ошибку в код ответа. Собрано в одном месте:
// разложенное по обработчикам, оно разъезжается на второй же операции.
func writeError(ctx context.Context, w http.ResponseWriter, message string, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "Not found", http.StatusNotFound)
	case errors.Is(err, ErrProductNotFound):
		http.Error(w, "Product not found", http.StatusNotFound)
	case errors.Is(err, ErrForbidden), errors.Is(err, wishlist.ErrForbiddenTransition):
		http.Error(w, "Forbidden", http.StatusForbidden)
	case errors.Is(err, wishlist.ErrInvalidTransition):
		// Состояние изменилось или переход невозможен: это конфликт,
		// а не ошибка в запросе — повтор с тем же телом ничего не даст.
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, ErrInsufficientFunds):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, ErrMarketplaceUnavailable), errors.Is(err, ErrWalletUnavailable):
		slog.WarnContext(ctx, message, slog.String("err", err.Error()))
		w.Header().Set("Retry-After", "5")
		http.Error(w, "Dependency is unavailable", http.StatusServiceUnavailable)
	default:
		slog.ErrorContext(ctx, message, slog.String("err", err.Error()))
		http.Error(w, message, http.StatusInternalServerError)
	}
}

func writeJSON(ctx context.Context, w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		// Ответ уже начат: сообщить об ошибке клиенту нечем.
		slog.ErrorContext(ctx, "Can't encode response", slog.String("err", err.Error()))
	}
}
