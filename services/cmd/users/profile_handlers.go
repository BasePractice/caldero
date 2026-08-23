package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"wish/services"

	"github.com/google/uuid"
)

// handlePublicProfile отдаёт карточку пользователя по идентификатору.
// Требует аутентификации: иначе идентификаторы, попадающие в ссылки
// и заголовки, превращаются в способ обойти систему целиком.
func (s *Service) handlePublicProfile(w http.ResponseWriter, r *http.Request) {
	if _, err := services.HttpAuthorized(r); err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid id", http.StatusBadRequest)
		return
	}

	user, err := s.db.GetUserById(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "Loading public profile", slog.String("err", err.Error()))
		http.Error(w, "Can't load profile", http.StatusInternalServerError)
		return
	}

	writeJSON(w, r, user.PublicProfile())
}

// handleProfile отдаёт полный профиль владельца токена.
func (s *Service) handleProfile(w http.ResponseWriter, r *http.Request) {
	user, ok := s.currentUser(w, r)
	if !ok {
		return
	}
	writeJSON(w, r, user.Profile())
}

// handleUpdateProfile меняет профиль владельца токена.
func (s *Service) handleUpdateProfile(w http.ResponseWriter, r *http.Request) {
	user, ok := s.currentUser(w, r)
	if !ok {
		return
	}

	update, err := services.DecodeJSON[ProfileUpdate](w, r)
	if err != nil {
		services.WriteDecodeError(w, err)
		return
	}

	if update.Phone != nil && *update.Phone != "" {
		normalized, err := NormalizePhone(*update.Phone)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		update.Phone = &normalized
	}
	if update.Phone != nil && *update.Phone == "" {
		// Телефон обязателен, поэтому очистить его нельзя.
		http.Error(w, "phone can not be cleared", http.StatusBadRequest)
		return
	}
	if update.Email != nil && *update.Email != "" {
		if err = ValidateEmail(*update.Email); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if update.Gender != nil && *update.Gender != "" {
		if err = ValidateGender(*update.Gender); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	updated, err := s.db.UpdateProfile(r.Context(), user.Id, update)
	if s.db.IsUniqueConstraintError(err) {
		http.Error(w, "Phone or email is already taken", http.StatusConflict)
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "Updating profile", slog.String("err", err.Error()))
		http.Error(w, "Can't update profile", http.StatusInternalServerError)
		return
	}

	writeJSON(w, r, updated.Profile())
}

// currentUser достаёт пользователя по subject проверенного токена.
func (s *Service) currentUser(w http.ResponseWriter, r *http.Request) (*User, bool) {
	requester, ok := requesterFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return nil, false
	}
	id, err := uuid.Parse(requester.GetSession().GetSubject())
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return nil, false
	}

	user, err := s.db.GetUserById(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Not found", http.StatusNotFound)
		return nil, false
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "Loading profile", slog.String("err", err.Error()))
		http.Error(w, "Can't load profile", http.StatusInternalServerError)
		return nil, false
	}
	return user, true
}

func writeJSON(w http.ResponseWriter, r *http.Request, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.ErrorContext(r.Context(), "Encoding response", slog.String("err", err.Error()))
	}
}
