//go:build integration

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"wish/services/testsupport"

	"github.com/google/uuid"
)

// fakeProvider изображает внешнего провайдера: обмен кода на токен
// и выдачу профиля. Настоящего провайдера в тестах нет, а поток обмена
// у всех одинаковый — различаются только адреса и имена полей.
type fakeProvider struct {
	server *httptest.Server
	// tokenStatus и profileStatus позволяют проверить ветки отказа:
	// провайдер отвечает не только успехом.
	tokenStatus   int
	profileStatus int
	externalId    string
	email         string
}

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	provider := &fakeProvider{
		tokenStatus: http.StatusOK, profileStatus: http.StatusOK,
		externalId: "ext-" + uuid.NewString()[:8], email: "social@example.com",
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, _ *http.Request) {
		if provider.tokenStatus != http.StatusOK {
			w.WriteHeader(provider.tokenStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"provider-token","token_type":"Bearer"}`))
	})
	mux.HandleFunc("GET /userinfo", func(w http.ResponseWriter, _ *http.Request) {
		if provider.profileStatus != http.StatusOK {
			w.WriteHeader(provider.profileStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id": provider.externalId, "email": provider.email, "name": "Пётр",
		})
	})

	provider.server = httptest.NewServer(mux)
	t.Cleanup(provider.server.Close)
	return provider
}

// newSocialHandlerService поднимает сервис с одним настроенным провайдером.
func newSocialHandlerService(t *testing.T, provider *fakeProvider) (*Service, http.Handler) {
	t.Helper()

	t.Setenv("SOCIAL_DEMO_CLIENT_ID", "demo-client")
	t.Setenv("SOCIAL_DEMO_CLIENT_SECRET", "demo-secret")
	t.Setenv("SOCIAL_DEMO_AUTH_URL", provider.server.URL+"/authorize")
	t.Setenv("SOCIAL_DEMO_TOKEN_URL", provider.server.URL+"/token")
	t.Setenv("SOCIAL_DEMO_USERINFO_URL", provider.server.URL+"/userinfo")

	cfg := testsupport.Prepare(t, "users")
	cfg.OAuth2GlobalSecret = "0123456789abcdef0123456789abcdef"
	cfg.AdminToken = adminToken
	cfg.SocialRedirectBase = "https://wish.example/api/v1"
	cfg.SocialProviders = []string{"demo"}

	service, err := newService(context.Background(), cfg)
	if err != nil {
		t.Fatalf("не удалось создать сервис: %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return service, registerHttpHandlers(service)
}

func get(handler http.Handler, path string, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// stateFrom достаёт одноразовое состояние из адреса, куда сервис
// отправляет пользователя: по нему провайдер вернётся обратно.
func stateFrom(t *testing.T, location string) string {
	t.Helper()
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatalf("разбор адреса провайдера: %v", err)
	}
	state := parsed.Query().Get("state")
	if state == "" {
		t.Fatalf("состояние не передано провайдеру: %s", location)
	}
	return state
}

// TestSocialStartAndCallback проходит внешний вход по HTTP целиком:
// начало, возврат от провайдера и заведение пользователя.
func TestSocialStartAndCallback(t *testing.T) {
	provider := newFakeProvider(t)
	service, handler := newSocialHandlerService(t, provider)

	start := get(handler, "/auth/social/demo", nil)
	if start.Code != http.StatusFound {
		t.Fatalf("код ответа %d (%s)", start.Code, start.Body)
	}
	state := stateFrom(t, start.Header().Get("Location"))

	callback := get(handler, "/auth/social/demo/callback?code=provider-code&state="+state, nil)
	if callback.Code != http.StatusOK {
		t.Fatalf("код ответа %d (%s)", callback.Code, callback.Body)
	}

	var result struct {
		Provider   string    `json:"provider"`
		ExternalId string    `json:"external_id"`
		UserId     uuid.UUID `json:"user_id"`
		Linked     bool      `json:"linked"`
	}
	if err := json.Unmarshal(callback.Body.Bytes(), &result); err != nil {
		t.Fatalf("разбор ответа: %v", err)
	}
	if result.Provider != "demo" || result.ExternalId != provider.externalId || !result.Linked {
		t.Fatalf("ответ: %+v", result)
	}

	identities, err := service.db.Identities(context.Background(), result.UserId)
	if err != nil {
		t.Fatalf("чтение идентичностей: %v", err)
	}
	if len(identities) != 1 {
		t.Errorf("идентичностей %d, ожидалась одна", len(identities))
	}

	t.Run("состояние одноразовое", func(t *testing.T) {
		// Повторно предъявленный ответ провайдера не должен срабатывать.
		again := get(handler, "/auth/social/demo/callback?code=provider-code&state="+state, nil)
		if again.Code != http.StatusBadRequest {
			t.Errorf("код ответа %d, ожидался %d", again.Code, http.StatusBadRequest)
		}
	})
}

func TestSocialCallbackErrors(t *testing.T) {
	provider := newFakeProvider(t)
	_, handler := newSocialHandlerService(t, provider)

	newState := func(t *testing.T) string {
		t.Helper()
		start := get(handler, "/auth/social/demo", nil)
		if start.Code != http.StatusFound {
			t.Fatalf("начало входа: %d (%s)", start.Code, start.Body)
		}
		return stateFrom(t, start.Header().Get("Location"))
	}

	t.Run("неизвестный провайдер", func(t *testing.T) {
		// Неизвестный и выключенный провайдер для клиента — одно и то же.
		if code := get(handler, "/auth/social/whatever", nil).Code; code != http.StatusNotFound {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusNotFound)
		}
		if code := get(handler, "/auth/social/whatever/callback?code=c&state=s", nil).Code; code != http.StatusNotFound {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusNotFound)
		}
	})

	t.Run("пользователь отказал в доступе", func(t *testing.T) {
		code := get(handler, "/auth/social/demo/callback?error=access_denied&state=s", nil).Code
		if code != http.StatusBadRequest {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusBadRequest)
		}
	})

	t.Run("без кода и состояния", func(t *testing.T) {
		if code := get(handler, "/auth/social/demo/callback", nil).Code; code != http.StatusBadRequest {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusBadRequest)
		}
	})

	t.Run("неизвестное состояние", func(t *testing.T) {
		code := get(handler, "/auth/social/demo/callback?code=c&state=нет-такого", nil).Code
		if code != http.StatusBadRequest {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusBadRequest)
		}
	})

	t.Run("провайдер отказал в обмене кода", func(t *testing.T) {
		state := newState(t)
		provider.tokenStatus = http.StatusInternalServerError
		defer func() { provider.tokenStatus = http.StatusOK }()

		code := get(handler, "/auth/social/demo/callback?code=c&state="+state, nil).Code
		if code != http.StatusServiceUnavailable {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusServiceUnavailable)
		}
	})

	t.Run("провайдер не отдал профиль", func(t *testing.T) {
		state := newState(t)
		provider.profileStatus = http.StatusForbidden
		defer func() { provider.profileStatus = http.StatusOK }()

		code := get(handler, "/auth/social/demo/callback?code=c&state="+state, nil).Code
		if code != http.StatusServiceUnavailable {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusServiceUnavailable)
		}
	})
}

// TestSocialWithoutRedirectBase: без базы адреса возврата вход невозможен,
// и это отказ настройки, а не ошибка пользователя.
func TestSocialWithoutRedirectBase(t *testing.T) {
	provider := newFakeProvider(t)
	t.Setenv("SOCIAL_DEMO_CLIENT_ID", "demo-client")
	t.Setenv("SOCIAL_DEMO_CLIENT_SECRET", "demo-secret")
	t.Setenv("SOCIAL_DEMO_AUTH_URL", provider.server.URL+"/authorize")
	t.Setenv("SOCIAL_DEMO_TOKEN_URL", provider.server.URL+"/token")
	t.Setenv("SOCIAL_DEMO_USERINFO_URL", provider.server.URL+"/userinfo")

	cfg := testsupport.Prepare(t, "users")
	cfg.OAuth2GlobalSecret = "0123456789abcdef0123456789abcdef"
	cfg.SocialProviders = []string{"demo"}

	service, err := newService(context.Background(), cfg)
	if err != nil {
		t.Fatalf("не удалось создать сервис: %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })

	code := get(registerHttpHandlers(service), "/auth/social/demo", nil).Code
	if code != http.StatusServiceUnavailable {
		t.Errorf("код ответа %d, ожидался %d", code, http.StatusServiceUnavailable)
	}
}

// TestSocialLoginContinuesAuthorization: вход через провайдера, начатый
// из потока авторизации, обязан этот поток продолжить и выдать код.
func TestSocialLoginContinuesAuthorization(t *testing.T) {
	provider := newFakeProvider(t)
	_, handler := newSocialHandlerService(t, provider)
	clientId := createClient(t, handler)

	authorizeQuery := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientId},
		"redirect_uri":          {redirectURI},
		"scope":                 {"openid read"},
		"state":                 {"state-9876543210"},
		"code_challenge":        {challenge()},
		"code_challenge_method": {"S256"},
	}.Encode()

	start := get(handler, "/auth/social/demo?"+authorizeQuery, nil)
	if start.Code != http.StatusFound {
		t.Fatalf("начало входа: %d (%s)", start.Code, start.Body)
	}
	state := stateFrom(t, start.Header().Get("Location"))

	callback := get(handler, "/auth/social/demo/callback?code=provider-code&state="+state, nil)
	if callback.Code != http.StatusFound && callback.Code != http.StatusSeeOther {
		t.Fatalf("код ответа %d (%s)", callback.Code, callback.Body)
	}
	location, err := url.Parse(callback.Header().Get("Location"))
	if err != nil {
		t.Fatalf("разбор Location: %v", err)
	}
	if location.Query().Get("code") == "" {
		t.Fatalf("код авторизации не выдан: %s", location)
	}
	if got := location.Query().Get("state"); got != "state-9876543210" {
		t.Errorf("state %q не вернулся клиенту", got)
	}
}

// TestIdentitiesAndLinking проверяет привязку и отвязку внешнего аккаунта
// владельцем токена.
func TestIdentitiesAndLinking(t *testing.T) {
	provider := newFakeProvider(t)
	_, handler := newSocialHandlerService(t, provider)
	clientId := createClient(t, handler)

	username := "user-" + uuid.NewString()[:8]
	registerViaAPI(t, handler, username, "+79002220011")
	token := tokenFor(t, handler, clientId, username)
	auth := map[string]string{"Authorization": "Bearer " + token}

	t.Run("сначала способов входа нет", func(t *testing.T) {
		recorder := get(handler, "/profile/identities", auth)
		if recorder.Code != http.StatusOK {
			t.Fatalf("код ответа %d (%s)", recorder.Code, recorder.Body)
		}
		var identities []Identity
		if err := json.Unmarshal(recorder.Body.Bytes(), &identities); err != nil {
			t.Fatalf("разбор ответа: %v", err)
		}
		if len(identities) != 0 {
			t.Errorf("идентичностей %d, ожидалось ноль", len(identities))
		}
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/profile/identities/demo", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("начало привязки: %d (%s)", recorder.Code, recorder.Body)
	}
	var link struct {
		AuthorizeURL string `json:"authorize_url"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &link); err != nil {
		t.Fatalf("разбор ответа: %v", err)
	}
	state := stateFrom(t, link.AuthorizeURL)

	callback := get(handler, "/auth/social/demo/callback?code=provider-code&state="+state, nil)
	if callback.Code != http.StatusOK {
		t.Fatalf("возврат от провайдера: %d (%s)", callback.Code, callback.Body)
	}

	t.Run("способ входа появился", func(t *testing.T) {
		recorder := get(handler, "/profile/identities", auth)
		var identities []Identity
		if err := json.Unmarshal(recorder.Body.Bytes(), &identities); err != nil {
			t.Fatalf("разбор ответа: %v", err)
		}
		if len(identities) != 1 || identities[0].Provider != "demo" {
			t.Errorf("идентичности: %+v", identities)
		}
	})

	t.Run("отвязка", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodDelete, "/profile/identities/demo", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		handler.ServeHTTP(recorder, request)
		// У пользователя есть пароль, поэтому отвязать последний внешний
		// способ входа можно: доступ не теряется.
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("код ответа %d (%s)", recorder.Code, recorder.Body)
		}
	})

	t.Run("отвязка того, чего нет", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodDelete, "/profile/identities/demo", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotFound {
			t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusNotFound)
		}
	})

	t.Run("привязка неизвестного провайдера", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/profile/identities/whatever", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotFound {
			t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusNotFound)
		}
	})
}

// tokenFor проходит вход по паролю и возвращает access-токен: часть тестов
// проверяет эндпоинты, доступные только владельцу токена.
func tokenFor(t *testing.T, handler http.Handler, clientId, username string) string {
	t.Helper()

	query := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientId},
		"redirect_uri":          {redirectURI},
		"scope":                 {"openid read"},
		"state":                 {"state-0123456789"},
		"code_challenge":        {challenge()},
		"code_challenge_method": {"S256"},
	}.Encode()

	recorder := form(handler, http.MethodPost, "/auth?"+query, url.Values{
		"username": {username}, "password": {"correct horse battery staple"},
	}, nil)
	if recorder.Code != http.StatusFound && recorder.Code != http.StatusSeeOther {
		t.Fatalf("вход: %d (%s)", recorder.Code, recorder.Body)
	}
	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatalf("разбор Location: %v", err)
	}
	code := location.Query().Get("code")
	if code == "" {
		t.Fatalf("код авторизации не выдан: %s", location)
	}

	token := exchange(t, handler, clientId, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	})
	if token.AccessToken == "" {
		t.Fatal("access-токен не выдан")
	}
	return token.AccessToken
}

// TestSocialIdentityTaken: внешний аккаунт, уже связанный с другим
// пользователем, привязать к себе нельзя — иначе чужой профиль
// захватывается вместе с ним.
func TestSocialIdentityTaken(t *testing.T) {
	provider := newFakeProvider(t)
	_, handler := newSocialHandlerService(t, provider)
	clientId := createClient(t, handler)

	// Первый вход заводит пользователя и связывает с ним внешний аккаунт.
	start := get(handler, "/auth/social/demo", nil)
	first := get(handler, "/auth/social/demo/callback?code=c&state="+
		stateFrom(t, start.Header().Get("Location")), nil)
	if first.Code != http.StatusOK {
		t.Fatalf("первый вход: %d (%s)", first.Code, first.Body)
	}

	username := "user-" + uuid.NewString()[:8]
	registerViaAPI(t, handler, username, "+79002220022")
	token := tokenFor(t, handler, clientId, username)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/profile/identities/demo", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("начало привязки: %d (%s)", recorder.Code, recorder.Body)
	}
	var link struct {
		AuthorizeURL string `json:"authorize_url"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &link); err != nil {
		t.Fatalf("разбор ответа: %v", err)
	}

	callback := get(handler, "/auth/social/demo/callback?code=c&state="+
		stateFrom(t, link.AuthorizeURL), nil)
	if callback.Code != http.StatusConflict {
		t.Errorf("код ответа %d, ожидался %d (%s)", callback.Code, http.StatusConflict, callback.Body)
	}
}

// TestLoginPageShowsSocialLinks: кнопки внешнего входа на форме сохраняют
// исходные параметры запроса — иначе после провайдера возвращаться некуда.
func TestLoginPageShowsSocialLinks(t *testing.T) {
	provider := newFakeProvider(t)
	_, handler := newSocialHandlerService(t, provider)
	clientId := createClient(t, handler)

	query := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientId},
		"redirect_uri":          {redirectURI},
		"scope":                 {"openid"},
		"state":                 {"state-0123456789"},
		"code_challenge":        {challenge()},
		"code_challenge_method": {"S256"},
	}.Encode()

	recorder := get(handler, "/auth?"+query, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("код ответа %d (%s)", recorder.Code, recorder.Body)
	}
	if !strings.Contains(recorder.Body.String(), "/auth/social/demo?") {
		t.Errorf("на форме нет ссылки внешнего входа: %s", recorder.Body)
	}
}
