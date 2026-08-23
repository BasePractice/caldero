package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// redirectURI собирает адрес, на который провайдер вернёт пользователя.
// Он обязан совпадать с тем, что зарегистрирован в приложении провайдера,
// поэтому база задаётся конфигурацией, а не выводится из запроса: заголовок
// Host подделывается, и по нему нельзя строить адрес возврата.
func (s *Service) redirectURI(provider string) string {
	if s.cfg.SocialRedirectBase == "" {
		return ""
	}
	return strings.TrimRight(s.cfg.SocialRedirectBase, "/") + "/auth/social/" + provider + "/callback"
}

// handleSocialStart начинает вход через внешнего провайдера.
func (s *Service) handleSocialStart(w http.ResponseWriter, r *http.Request) {
	provider, ok := s.socialProvider(w, r)
	if !ok {
		return
	}

	// Исходный запрос авторизации сохраняется целиком: после возвращения
	// от провайдера продолжается ровно тот поток, который начал клиент.
	target, err := s.startSocialLogin(r, provider, r.URL.RawQuery, nil)
	if err != nil {
		writeSocialError(r.Context(), w, err)
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// handleSocialCallback принимает ответ провайдера.
func (s *Service) handleSocialCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	provider, ok := s.socialProvider(w, r)
	if !ok {
		return
	}

	if failure := r.URL.Query().Get("error"); failure != "" {
		// Пользователь отказал в доступе — это не сбой системы.
		slog.DebugContext(ctx, "Social login refused",
			slog.String("provider", provider.Name), slog.String("error", failure))
		http.Error(w, "Social login was refused", http.StatusBadRequest)
		return
	}

	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" || state == "" {
		http.Error(w, "code and state are required", http.StatusBadRequest)
		return
	}

	// Состояние одноразовое: оно удаляется при чтении, поэтому повторно
	// предъявленный ответ провайдера не сработает.
	login, err := s.db.TakeSocialLogin(ctx, state)
	if err != nil {
		writeSocialError(ctx, w, err)
		return
	}
	if login.Provider != provider.Name {
		http.Error(w, "State belongs to another provider", http.StatusBadRequest)
		return
	}

	token, err := s.social.Exchange(ctx, provider, code, login.Verifier, s.redirectURI(provider.Name))
	if err != nil {
		writeSocialError(ctx, w, err)
		return
	}
	profile, err := s.social.Profile(ctx, provider, token)
	if err != nil {
		writeSocialError(ctx, w, err)
		return
	}

	user, err := s.resolveIdentity(ctx, login, profile)
	if err != nil {
		writeSocialError(ctx, w, err)
		return
	}

	if login.AuthorizeQuery == "" {
		// Вход начат вне потока авторизации — например, ради привязки.
		writeJSON(w, r, map[string]any{
			"provider":    profile.Provider,
			"external_id": profile.ExternalId,
			"user_id":     user,
			"linked":      true,
		})
		return
	}
	s.completeAuthorization(w, r, login.AuthorizeQuery, user)
}

// resolveIdentity решает, кому принадлежит внешний аккаунт.
//
// Автоматическое связывание по совпадению почты не делается: почта
// у провайдера может быть не подтверждена, и тогда чужой профиль
// захватывается тем, кто просто завёл ящик с таким адресом.
func (s *Service) resolveIdentity(
	ctx context.Context,
	login SocialLogin,
	profile SocialProfile,
) (uuid.UUID, error) {
	existing, err := s.db.IdentityUser(ctx, profile.Provider, profile.ExternalId)
	switch {
	case err == nil:
		if login.LinkUserId != nil && *login.LinkUserId != existing {
			return uuid.Nil, ErrIdentityTaken
		}
		return existing, nil
	case !errors.Is(err, sql.ErrNoRows):
		return uuid.Nil, err
	}

	if login.LinkUserId != nil {
		if err = s.db.LinkIdentity(ctx, *login.LinkUserId, profile); err != nil {
			return uuid.Nil, err
		}
		return *login.LinkUserId, nil
	}

	created, err := s.db.CreateSocialUser(ctx, profile)
	if err != nil {
		return uuid.Nil, err
	}
	slog.InfoContext(ctx, "User created from social identity",
		slog.String("provider", profile.Provider), slog.String("user", created.Id.String()))
	return created.Id, nil
}

// handleIdentities отдаёт способы внешнего входа владельца токена.
func (s *Service) handleIdentities(w http.ResponseWriter, r *http.Request) {
	user, ok := s.currentUser(w, r)
	if !ok {
		return
	}

	identities, err := s.db.Identities(r.Context(), user.Id)
	if err != nil {
		slog.ErrorContext(r.Context(), "Loading identities", slog.String("err", err.Error()))
		http.Error(w, "Can't load identities", http.StatusInternalServerError)
		return
	}
	writeJSON(w, r, identities)
}

// handleLinkIdentity начинает привязку внешнего аккаунта к текущему.
func (s *Service) handleLinkIdentity(w http.ResponseWriter, r *http.Request) {
	user, ok := s.currentUser(w, r)
	if !ok {
		return
	}
	provider, ok := s.socialProvider(w, r)
	if !ok {
		return
	}

	// Запрос авторизации не сохраняется: пользователь уже вошёл,
	// и после привязки его некуда возвращать по протоколу.
	target, err := s.startSocialLogin(r, provider, "", &user.Id)
	if err != nil {
		writeSocialError(r.Context(), w, err)
		return
	}
	writeJSON(w, r, map[string]string{"authorize_url": target})
}

// handleUnlinkIdentity отвязывает внешний аккаунт.
func (s *Service) handleUnlinkIdentity(w http.ResponseWriter, r *http.Request) {
	user, ok := s.currentUser(w, r)
	if !ok {
		return
	}

	provider := strings.ToLower(r.PathValue("provider"))
	err := s.db.UnlinkIdentity(r.Context(), user.Id, provider)
	switch {
	case errors.Is(err, ErrLastIdentity):
		// Отвязать последний способ входа значит отобрать доступ.
		http.Error(w, "This is the only way to sign in", http.StatusConflict)
	case errors.Is(err, sql.ErrNoRows):
		http.Error(w, "Not found", http.StatusNotFound)
	case err != nil:
		slog.ErrorContext(r.Context(), "Unlinking identity", slog.String("err", err.Error()))
		http.Error(w, "Can't unlink identity", http.StatusInternalServerError)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// startSocialLogin заводит состояние входа и собирает адрес провайдера.
func (s *Service) startSocialLogin(
	r *http.Request,
	provider SocialProvider,
	authorizeQuery string,
	link *uuid.UUID,
) (string, error) {
	redirect := s.redirectURI(provider.Name)
	if redirect == "" {
		return "", fmt.Errorf("%w: SOCIAL_REDIRECT_BASE is empty", ErrUnknownProvider)
	}

	state, verifier, challenge, err := NewSocialSecrets()
	if err != nil {
		return "", err
	}
	if err = s.db.StartSocialLogin(r.Context(), SocialLogin{
		State:          state,
		Provider:       provider.Name,
		Verifier:       verifier,
		AuthorizeQuery: authorizeQuery,
		LinkUserId:     link,
		ExpiresAt:      time.Now().Add(socialStateTTL),
	}); err != nil {
		return "", err
	}
	return provider.AuthorizeURL(redirect, state, challenge), nil
}

// completeAuthorization продолжает поток авторизации от имени
// установленного пользователя.
func (s *Service) completeAuthorization(
	w http.ResponseWriter,
	r *http.Request,
	authorizeQuery string,
	userId uuid.UUID,
) {
	ctx := r.Context()

	// Запрос авторизации разбирается заново из сохранённой строки:
	// fosite читает параметры из URL, а исходный запрос давно завершён.
	original := r.Clone(ctx)
	original.Method = http.MethodGet
	original.URL = &url.URL{Path: "/auth", RawQuery: authorizeQuery}
	original.Form = nil
	original.PostForm = nil

	authorizeRequest, err := s.oauth2Provider.NewAuthorizeRequest(ctx, original)
	if err != nil {
		slog.DebugContext(ctx, "Authorize request rejected after social login",
			slog.String("err", err.Error()))
		s.oauth2Provider.WriteAuthorizeError(ctx, w, authorizeRequest, err)
		return
	}

	// Согласие выражено самим фактом входа через провайдера, поэтому
	// выдаются ровно запрошенные scope, не больше.
	for _, scope := range authorizeRequest.GetRequestedScopes() {
		authorizeRequest.GrantScope(scope)
	}
	for _, audience := range authorizeRequest.GetRequestedAudience() {
		authorizeRequest.GrantAudience(audience)
	}

	session := s.newSession(ctx, userId)
	s.applyRoles(ctx, session)

	response, err := s.oauth2Provider.NewAuthorizeResponse(ctx, authorizeRequest, session)
	if err != nil {
		slog.DebugContext(ctx, "Authorize response failed", slog.String("err", err.Error()))
		s.oauth2Provider.WriteAuthorizeError(ctx, w, authorizeRequest, err)
		return
	}
	s.oauth2Provider.WriteAuthorizeResponse(ctx, w, authorizeRequest, response)
}

// socialProvider достаёт провайдера из пути.
func (s *Service) socialProvider(w http.ResponseWriter, r *http.Request) (SocialProvider, bool) {
	name := strings.ToLower(r.PathValue("provider"))
	provider, ok := s.providers[name]
	if !ok {
		// Неизвестный провайдер и выключенный — одно и то же для клиента.
		http.Error(w, "Unknown social provider", http.StatusNotFound)
		return SocialProvider{}, false
	}
	return provider, true
}

func writeSocialError(ctx context.Context, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrUnknownProvider):
		http.Error(w, "Social login is not configured", http.StatusServiceUnavailable)
	case errors.Is(err, ErrSocialState):
		// Подробности не раскрываются: для пользователя это «начните заново».
		http.Error(w, "Social login expired, start again", http.StatusBadRequest)
	case errors.Is(err, ErrIdentityTaken):
		http.Error(w, "This account is already linked to another user", http.StatusConflict)
	case errors.Is(err, ErrSocialProvider):
		slog.WarnContext(ctx, "Social provider failed", slog.String("err", err.Error()))
		http.Error(w, "Social provider is unavailable", http.StatusServiceUnavailable)
	default:
		slog.ErrorContext(ctx, "Social login failed", slog.String("err", err.Error()))
		http.Error(w, "Social login failed", http.StatusInternalServerError)
	}
}
