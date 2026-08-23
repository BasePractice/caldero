package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"wish/services"
	"wish/services/shared/notify"
)

// RequestConfirmation выдаёт код подтверждения и отправляет его владельцу
// контакта.
//
// Код уходит через сервис оповещений: заводить в этом сервисе вторую
// отправку сообщений значило бы дублировать и очередь, и повторы,
// и ограничение частоты доставки.
func (s *Service) RequestConfirmation(
	ctx context.Context,
	user *User,
	kind ConfirmationKind,
) (Confirmation, error) {
	target, confirmed := user.Contact(kind)
	if target == "" {
		return Confirmation{}, fmt.Errorf("%w: %s is empty", ErrNoContact, kind)
	}
	if confirmed {
		return Confirmation{}, fmt.Errorf("%w: %s", ErrAlreadyConfirmed, kind)
	}

	// Ограничение частоты обязательно: без него отправка кода становится
	// способом слать сообщения на чужой номер за наш счёт.
	count, last, err := s.db.CountConfirmations(ctx, user.Id, kind, s.cfg.ConfirmationRateWindow)
	if err != nil {
		return Confirmation{}, err
	}
	if count >= s.cfg.ConfirmationRateLimit {
		return Confirmation{}, fmt.Errorf("%w: %d codes within %s",
			ErrTooOften, count, s.cfg.ConfirmationRateWindow)
	}
	if !last.IsZero() && time.Since(last) < s.cfg.ConfirmationCooldown {
		return Confirmation{}, fmt.Errorf("%w: next code in %s",
			ErrTooOften, (s.cfg.ConfirmationCooldown - time.Since(last)).Round(time.Second))
	}

	code, err := NewCode(kind)
	if err != nil {
		return Confirmation{}, err
	}
	confirmation, err := s.db.CreateConfirmation(ctx, Confirmation{
		UserId:    user.Id,
		Kind:      kind,
		Target:    target,
		CodeHash:  CodeHash(s.confirmationSecret, user.Id, kind, target, code),
		ExpiresAt: time.Now().Add(s.cfg.ConfirmationTTL),
	})
	if err != nil {
		return Confirmation{}, err
	}

	s.deliver(ctx, user, kind, code)
	return confirmation, nil
}

// deliver отправляет код. Сбой доставки не отменяет выданный код:
// пользователь может запросить его повторно, а вот выдать код и забыть
// о нём было бы хуже.
func (s *Service) deliver(ctx context.Context, user *User, kind ConfirmationKind, code string) {
	if !s.notifier.Enabled() {
		slog.WarnContext(ctx, "Confirmation code is not delivered: notifications are disabled",
			slog.String("user", user.Id.String()), slog.String("kind", string(kind)))
		return
	}

	minutes := strconv.Itoa(int(s.cfg.ConfirmationTTL.Minutes()))
	event := notify.PublishEvent{
		UserId: user.Id,
		Type:   notify.EventConfirmationCode,
		// Ключ дедупликации не задаётся: каждый запрошенный код —
		// отдельное сообщение, и схлопывать их нельзя.
		Payload: map[string]string{"code": code, "minutes": minutes},
	}
	if link := ConfirmationLink(s.cfg.PublicBaseURL, kind, code); kind == ConfirmEmail && link != "" {
		event.Type = notify.EventConfirmationLink
		event.Payload = map[string]string{"link": link, "minutes": minutes}
	}

	if err := s.notifier.Publish(ctx, event); err != nil {
		slog.ErrorContext(ctx, "Can't deliver confirmation code",
			slog.String("user", user.Id.String()), slog.String("err", err.Error()))
	}
}

// VerifyConfirmation проверяет предъявленный код.
func (s *Service) VerifyConfirmation(
	ctx context.Context,
	user *User,
	kind ConfirmationKind,
	code string,
) error {
	target, confirmed := user.Contact(kind)
	if confirmed {
		return fmt.Errorf("%w: %s", ErrAlreadyConfirmed, kind)
	}

	confirmation, err := s.db.ActiveConfirmation(ctx, user.Id, kind)
	if err != nil {
		return err
	}
	// Контакт мог смениться после отправки: код от старого номера
	// не должен подтверждать новый.
	if confirmation.Target != target {
		return ErrTargetChanged
	}

	if !MatchCode(s.confirmationSecret, confirmation, code) {
		// Попытка засчитывается даже при сбое записи: иначе счётчик
		// попыток обходится тем, что база отвечает с ошибкой.
		if err = s.db.FailConfirmation(ctx, confirmation.Id); err != nil {
			slog.ErrorContext(ctx, "Can't count failed attempt", slog.String("err", err.Error()))
		}
		return ErrWrongCode
	}
	return s.db.ConfirmContact(ctx, confirmation.Id, user.Id, kind, target)
}

// handleRequestConfirmation принимает запрос на отправку кода.
func (s *Service) handleRequestConfirmation(w http.ResponseWriter, r *http.Request) {
	user, ok := s.currentUser(w, r)
	if !ok {
		return
	}

	request, err := services.DecodeJSON[struct {
		Kind ConfirmationKind `json:"kind"`
	}](w, r)
	if err != nil {
		services.WriteDecodeError(w, err)
		return
	}
	if !request.Kind.Valid() {
		http.Error(w, "kind must be PHONE or EMAIL", http.StatusBadRequest)
		return
	}

	confirmation, err := s.RequestConfirmation(r.Context(), user, request.Kind)
	if err != nil {
		writeConfirmationError(r.Context(), w, err)
		return
	}

	// Сам код в ответе не возвращается: он должен прийти на контакт,
	// иначе подтверждение ничего не подтверждает.
	writeJSON(w, r, map[string]any{
		"kind":       confirmation.Kind,
		"expires_at": confirmation.ExpiresAt,
		"attempts":   MaxAttempts,
	})
}

// handleVerifyConfirmation принимает предъявленный код.
func (s *Service) handleVerifyConfirmation(w http.ResponseWriter, r *http.Request) {
	user, ok := s.currentUser(w, r)
	if !ok {
		return
	}

	request, err := services.DecodeJSON[struct {
		Kind ConfirmationKind `json:"kind"`
		Code string           `json:"code"`
	}](w, r)
	if err != nil {
		services.WriteDecodeError(w, err)
		return
	}
	if !request.Kind.Valid() {
		http.Error(w, "kind must be PHONE or EMAIL", http.StatusBadRequest)
		return
	}
	if request.Code == "" {
		http.Error(w, "code is required", http.StatusBadRequest)
		return
	}

	if err = s.VerifyConfirmation(r.Context(), user, request.Kind, request.Code); err != nil {
		writeConfirmationError(r.Context(), w, err)
		return
	}

	updated, err := s.db.GetUserById(r.Context(), user.Id)
	if err != nil {
		slog.ErrorContext(r.Context(), "Loading profile after confirmation",
			slog.String("err", err.Error()))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, r, updated.Profile())
}

func writeConfirmationError(ctx context.Context, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNoContact):
		http.Error(w, "Contact is not set in the profile", http.StatusBadRequest)
	case errors.Is(err, ErrAlreadyConfirmed):
		http.Error(w, "Contact is already confirmed", http.StatusConflict)
	case errors.Is(err, ErrTooOften):
		// Ограничение частоты — не ошибка запроса: клиенту нужно
		// подождать, и это ровно 429.
		w.Header().Set("Retry-After", "60")
		http.Error(w, err.Error(), http.StatusTooManyRequests)
	case errors.Is(err, ErrNoConfirmation):
		http.Error(w, "No active confirmation: request a new code", http.StatusConflict)
	case errors.Is(err, ErrTargetChanged):
		http.Error(w, "Contact has changed: request a new code", http.StatusConflict)
	case errors.Is(err, ErrWrongCode):
		// Неверный код и отсутствующий различаются намеренно: угадать
		// сам код это не помогает, а пользователю подсказывает, что делать.
		http.Error(w, "Code does not match", http.StatusBadRequest)
	default:
		slog.ErrorContext(ctx, "Confirmation failed", slog.String("err", err.Error()))
		http.Error(w, "Can't process confirmation", http.StatusInternalServerError)
	}
}
