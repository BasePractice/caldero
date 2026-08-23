package main

import (
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/ory/fosite"
)

// loginPage — форма входа для Authorization Code Flow. Страница отдаётся
// на GET /auth и отправляет учётные данные обратно на тот же адрес,
// сохраняя исходные параметры запроса в строке запроса.
//
// html/template, а не text/template: значения подставляются в разметку,
// и без экранирования сюда попадала бы произвольная строка из запроса.
var loginPage = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="ru">
<head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <title>Вход</title>
    <style>
        body { font-family: system-ui, sans-serif; max-width: 22rem; margin: 4rem auto; padding: 0 1rem; }
        label { display: block; margin-top: 1rem; }
        input { width: 100%; padding: .5rem; box-sizing: border-box; }
        button { margin-top: 1.5rem; padding: .6rem 1.2rem; }
        .error { color: #b00020; margin-top: 1rem; }
        .client { color: #555; font-size: .9rem; }
    </style>
</head>
<body>
<h1>Вход</h1>
<p class="client">Приложение <strong>{{ .ClientId }}</strong> запрашивает доступ{{ if .Scopes }} к: {{ .Scopes }}{{ end }}.</p>
{{ if .Error }}<p class="error">{{ .Error }}</p>{{ end }}
<form method="POST" action="{{ .Action }}">
    <label>Имя пользователя
        <input type="text" name="username" autocomplete="username" autofocus required>
    </label>
    <label>Пароль
        <input type="password" name="password" autocomplete="current-password" required>
    </label>
    <button type="submit">Войти и разрешить</button>
</form>
{{ if .Providers }}
<p class="client">Или войдите через:</p>
<ul>
    {{ range .Providers }}<li><a href="{{ .URL }}">{{ .Name }}</a></li>{{ end }}
</ul>
{{ end }}
</body>
</html>`))

type loginPageData struct {
	Action   string
	ClientId string
	Scopes   string
	Error    string
	// Providers — внешние способы входа. Ссылка сохраняет исходные
	// параметры запроса: после возвращения от провайдера продолжается
	// тот же поток авторизации.
	Providers []socialLink
}

// socialLink — кнопка входа через провайдера.
type socialLink struct {
	Name string
	URL  string
}

// socialLinks собирает ссылки входа для формы. Порядок фиксирован:
// карта провайдеров обходится в произвольном порядке, и без сортировки
// кнопки прыгали бы при каждом показе страницы.
func (s *Service) socialLinks(query string) []socialLink {
	if s.cfg.SocialRedirectBase == "" {
		return nil
	}

	names := make([]string, 0, len(s.providers))
	for name := range s.providers {
		names = append(names, name)
	}
	sort.Strings(names)

	links := make([]socialLink, 0, len(names))
	for _, name := range names {
		links = append(links, socialLink{
			Name: name,
			URL:  "/auth/social/" + name + "?" + query,
		})
	}
	return links
}

// handleAuthorization реализует Authorization Code Flow. Раньше эндпоинта
// не было вовсе: фабрика была отключена, а хранилище кодов паниковало.
func (s *Service) handleAuthorization(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	authorizeRequest, err := s.oauth2Provider.NewAuthorizeRequest(ctx, r)
	if err != nil {
		slog.DebugContext(ctx, "Authorize request rejected", slog.String("err", err.Error()))
		s.oauth2Provider.WriteAuthorizeError(ctx, w, authorizeRequest, err)
		return
	}

	page := loginPageData{
		// Действие формы сохраняет исходные параметры: fosite разбирает
		// запрос заново и на POST, а тело формы их не содержит.
		Action:    "?" + r.URL.RawQuery,
		ClientId:  authorizeRequest.GetClient().GetID(),
		Scopes:    strings.Join(authorizeRequest.GetRequestedScopes(), ", "),
		Providers: s.socialLinks(r.URL.RawQuery),
	}

	if r.Method != http.MethodPost {
		s.renderLogin(w, page, http.StatusOK)
		return
	}

	subject, err := s.db.Authenticate(ctx, r.PostFormValue("username"), r.PostFormValue("password"))
	if err != nil {
		if errors.Is(err, fosite.ErrNotFound) {
			// Одинаковый текст на неизвестного пользователя и неверный
			// пароль: различие позволяет перебирать логины.
			page.Error = "Неверное имя пользователя или пароль"
			s.renderLogin(w, page, http.StatusUnauthorized)
			return
		}
		slog.ErrorContext(ctx, "Authentication failed", slog.String("err", err.Error()))
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	userId, err := uuid.Parse(subject)
	if err != nil {
		slog.ErrorContext(ctx, "Authenticated subject is not a uuid", slog.String("err", err.Error()))
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Согласие пользователя выражено отправкой формы, поэтому выдаются
	// ровно запрошенные scope, не больше.
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

func (s *Service) renderLogin(w http.ResponseWriter, page loginPageData, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Форма входа не должна попадать в кэш промежуточных узлов.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := loginPage.Execute(w, page); err != nil {
		slog.Error("Rendering login page", slog.String("err", err.Error()))
	}
}
