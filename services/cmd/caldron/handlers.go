package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"wish/services"
	"wish/services/shared/caldron"
	"wish/services/shared/credit"

	"github.com/google/uuid"
)

type api struct {
	caldrons *Caldrons
}

func registerHttpHandlers(caldrons *Caldrons) http.Handler {
	a := &api{caldrons: caldrons}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /caldrons", a.create)
	mux.HandleFunc("GET /caldrons", a.list)
	mux.HandleFunc("GET /caldrons/{id}", a.caldron)
	mux.HandleFunc("POST /caldrons/{id}/participants", a.addParticipant)
	mux.HandleFunc("DELETE /caldrons/{id}/participants/{user}", a.removeParticipant)
	mux.HandleFunc("POST /caldrons/{id}/contribute", a.contribute)
	mux.HandleFunc("POST /caldrons/{id}/cancel", a.cancel)
	mux.HandleFunc("POST /caldrons/{id}/settle", a.settle)
	mux.HandleFunc("PUT /caldrons/{id}/gifts", a.setGifts)
	mux.HandleFunc("GET /caldrons/{id}/gifts", a.gifts)
	mux.HandleFunc("PUT /caldrons/{id}/arbiter", a.setArbiter)
	mux.HandleFunc("POST /caldrons/{id}/draw", a.draw)
	mux.HandleFunc("GET /caldrons/{id}/draw", a.drawResult)
	return services.Measure("caldron", mux)
}

func (a *api) create(w http.ResponseWriter, r *http.Request) {
	authorized, err := services.HttpAuthorized(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	create, err := services.DecodeJSON[caldron.CreateCaldron](w, r)
	if err != nil {
		services.WriteDecodeError(w, err)
		return
	}
	if err = create.Validate(); err != nil {
		slog.DebugContext(r.Context(), "Caldron validation failed",
			slog.String("caldron", create.String()), slog.String("reason", err.Error()))
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	pot, err := a.caldrons.Create(r.Context(), authorized.Id, create)
	if err != nil {
		writeError(r.Context(), w, "Can't create caldron", err)
		return
	}
	w.Header().Set("X-Caldron-Id", pot.Id.String())
	writeJSON(r.Context(), w, http.StatusCreated, pot)
}

func (a *api) list(w http.ResponseWriter, r *http.Request) {
	authorized, err := services.HttpAuthorized(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	caldrons, err := a.caldrons.List(r.Context(), authorized.Id)
	if err != nil {
		writeError(r.Context(), w, "Can't load caldrons", err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, caldrons)
}

func (a *api) caldron(w http.ResponseWriter, r *http.Request) {
	authorized, id, ok := a.request(w, r)
	if !ok {
		return
	}

	pot, err := a.caldrons.Caldron(r.Context(), authorized.Id, id)
	if err != nil {
		writeError(r.Context(), w, "Can't load caldron", err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, pot)
}

func (a *api) addParticipant(w http.ResponseWriter, r *http.Request) {
	authorized, id, ok := a.request(w, r)
	if !ok {
		return
	}

	add, err := services.DecodeJSON[caldron.AddParticipant](w, r)
	if err != nil {
		services.WriteDecodeError(w, err)
		return
	}

	pot, err := a.caldrons.AddParticipant(r.Context(), authorized.Id, id, add)
	if err != nil {
		writeError(r.Context(), w, "Can't add participant", err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, pot)
}

func (a *api) removeParticipant(w http.ResponseWriter, r *http.Request) {
	authorized, id, ok := a.request(w, r)
	if !ok {
		return
	}
	user, err := uuid.Parse(r.PathValue("user"))
	if err != nil {
		http.Error(w, "Invalid user id", http.StatusBadRequest)
		return
	}

	pot, err := a.caldrons.RemoveParticipant(r.Context(), authorized.Id, id, user)
	if err != nil {
		writeError(r.Context(), w, "Can't remove participant", err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, pot)
}

func (a *api) contribute(w http.ResponseWriter, r *http.Request) {
	authorized, id, ok := a.request(w, r)
	if !ok {
		return
	}

	// Тело необязательно: сумма нужна только в режиме диапазона.
	var contribution struct {
		Amount credit.Amount `json:"amount,omitempty"`
	}
	if r.ContentLength > 0 {
		decoded, err := services.DecodeJSON[struct {
			Amount credit.Amount `json:"amount,omitempty"`
		}](w, r)
		if err != nil {
			services.WriteDecodeError(w, err)
			return
		}
		contribution.Amount = decoded.Amount
	}

	pot, err := a.caldrons.Contribute(r.Context(), authorized.Id, id, contribution.Amount)
	if err != nil {
		writeError(r.Context(), w, "Can't contribute", err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, pot)
}

func (a *api) cancel(w http.ResponseWriter, r *http.Request) {
	authorized, id, ok := a.request(w, r)
	if !ok {
		return
	}

	pot, err := a.caldrons.Cancel(r.Context(), authorized.Id, id)
	if err != nil {
		writeError(r.Context(), w, "Can't cancel caldron", err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, pot)
}

func (a *api) settle(w http.ResponseWriter, r *http.Request) {
	authorized, id, ok := a.request(w, r)
	if !ok {
		return
	}

	settle, err := services.DecodeJSON[struct {
		Winner uuid.UUID `json:"winner"`
	}](w, r)
	if err != nil {
		services.WriteDecodeError(w, err)
		return
	}
	if settle.Winner == uuid.Nil {
		http.Error(w, "winner is required", http.StatusBadRequest)
		return
	}

	pot, err := a.caldrons.Settle(r.Context(), authorized.Id, id, settle.Winner)
	if err != nil {
		writeError(r.Context(), w, "Can't settle caldron", err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, pot)
}

// setGifts заменяет список подарков участника целиком: ограничение
// «не дороже суммы котла» относится к списку целиком.
func (a *api) setGifts(w http.ResponseWriter, r *http.Request) {
	authorized, id, ok := a.request(w, r)
	if !ok {
		return
	}

	requests, err := services.DecodeJSON[[]GiftRequest](w, r)
	if err != nil {
		services.WriteDecodeError(w, err)
		return
	}

	gifts, err := a.caldrons.SetGifts(r.Context(), authorized.Id, id, requests)
	if err != nil {
		writeError(r.Context(), w, "Can't set gifts", err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, gifts)
}

func (a *api) gifts(w http.ResponseWriter, r *http.Request) {
	authorized, id, ok := a.request(w, r)
	if !ok {
		return
	}

	gifts, err := a.caldrons.Gifts(r.Context(), authorized.Id, id)
	if err != nil {
		writeError(r.Context(), w, "Can't load gifts", err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, gifts)
}

func (a *api) setArbiter(w http.ResponseWriter, r *http.Request) {
	authorized, id, ok := a.request(w, r)
	if !ok {
		return
	}

	body, err := services.DecodeJSON[struct {
		UserId uuid.UUID `json:"user_id"`
	}](w, r)
	if err != nil {
		services.WriteDecodeError(w, err)
		return
	}
	if body.UserId == uuid.Nil {
		http.Error(w, "user_id is required", http.StatusBadRequest)
		return
	}

	pot, err := a.caldrons.SetArbiter(r.Context(), authorized.Id, id, body.UserId)
	if err != nil {
		writeError(r.Context(), w, "Can't set arbiter", err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, pot)
}

func (a *api) draw(w http.ResponseWriter, r *http.Request) {
	authorized, id, ok := a.request(w, r)
	if !ok {
		return
	}

	draw, err := a.caldrons.Draw(r.Context(), authorized.Id, id)
	if err != nil {
		writeError(r.Context(), w, "Can't draw", err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, draw)
}

func (a *api) drawResult(w http.ResponseWriter, r *http.Request) {
	authorized, id, ok := a.request(w, r)
	if !ok {
		return
	}

	draw, err := a.caldrons.DrawResult(r.Context(), authorized.Id, id)
	if err != nil {
		writeError(r.Context(), w, "Can't load draw", err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, draw)
}

// request разбирает то, что нужно каждому обработчику: кто спрашивает
// и про какой котёл.
func (a *api) request(w http.ResponseWriter, r *http.Request) (*services.AuthorizedUser, uuid.UUID, bool) {
	authorized, err := services.HttpAuthorized(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return nil, uuid.Nil, false
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid id", http.StatusBadRequest)
		return nil, uuid.Nil, false
	}
	return authorized, id, true
}

// writeError переводит доменную ошибку в код ответа.
func writeError(ctx context.Context, w http.ResponseWriter, message string, err error) {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrParticipantNotFound),
		errors.Is(err, ErrNoDraw), errors.Is(err, ErrProductNotFound):
		http.Error(w, "Not found", http.StatusNotFound)
	case errors.Is(err, ErrForbidden), errors.Is(err, caldron.ErrForbiddenTransition):
		http.Error(w, "Forbidden", http.StatusForbidden)
	case errors.Is(err, caldron.ErrInvalidContribution),
		errors.Is(err, caldron.ErrTooManyGifts), errors.Is(err, caldron.ErrGiftsTooExpensive):
		// Сумма не подходит под правила котла — это ошибка в запросе.
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrAlreadyPaid), errors.Is(err, ErrNotReady),
		errors.Is(err, ErrDrawRequired), errors.Is(err, caldron.ErrInvalidTransition):
		// Состояние котла не позволяет операцию: повтор с тем же телом
		// ничего не изменит.
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, ErrWalletUnavailable), errors.Is(err, ErrMarketplaceUnavailable):
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
