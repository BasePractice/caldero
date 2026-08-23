package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func testProvider(t *testing.T, server *httptest.Server) SocialProvider {
	t.Helper()
	return SocialProvider{
		Name:         "test",
		ClientId:     "client",
		ClientSecret: "secret",
		AuthURL:      server.URL + "/authorize",
		TokenURL:     server.URL + "/token",
		UserInfoURL:  server.URL + "/userinfo",
		Scopes:       "profile email",
		IdField:      "id",
		EmailField:   "email",
		NameField:    "name",
	}
}

func TestSocialProviderValidate(t *testing.T) {
	full := SocialProvider{
		Name: "test", ClientId: "a", ClientSecret: "b",
		AuthURL: "c", TokenURL: "d", UserInfoURL: "e",
	}
	if err := full.Validate(); err != nil {
		t.Errorf("полный провайдер отклонён: %v", err)
	}

	// Провайдер без адреса токена не заработает, и узнать об этом лучше
	// при старте, а не при первом входе пользователя.
	partial := full
	partial.TokenURL = ""
	if err := partial.Validate(); err == nil {
		t.Error("провайдер без адреса токена принят")
	}
	if err := partial.Validate(); err != nil && !strings.Contains(err.Error(), "TOKEN_URL") {
		t.Errorf("в ошибке не сказано, чего не хватает: %v", err)
	}
}

func TestAuthorizeURL(t *testing.T) {
	provider := SocialProvider{
		Name: "test", ClientId: "client", AuthURL: "https://provider.invalid/oauth",
		Scopes: "profile email",
	}
	target := provider.AuthorizeURL("https://wish.example/callback", "state-1", "challenge-1")

	parsed, err := url.Parse(target)
	if err != nil {
		t.Fatalf("разбор адреса: %v", err)
	}
	query := parsed.Query()
	for key, want := range map[string]string{
		"response_type":         "code",
		"client_id":             "client",
		"redirect_uri":          "https://wish.example/callback",
		"state":                 "state-1",
		"scope":                 "profile email",
		"code_challenge":        "challenge-1",
		"code_challenge_method": "S256",
	} {
		if got := query.Get(key); got != want {
			t.Errorf("%s = %q, ожидалось %q", key, got, want)
		}
	}

	t.Run("существующие параметры не затираются", func(t *testing.T) {
		withQuery := provider
		withQuery.AuthURL = "https://provider.invalid/oauth?display=popup"
		target := withQuery.AuthorizeURL("https://wish.example/callback", "s", "c")
		if !strings.Contains(target, "display=popup") {
			t.Errorf("параметр провайдера потерян: %s", target)
		}
	})
}

func TestNewSocialSecrets(t *testing.T) {
	state, verifier, challenge, err := NewSocialSecrets()
	if err != nil {
		t.Fatalf("генерация: %v", err)
	}
	if state == "" || verifier == "" || challenge == "" {
		t.Fatal("пустые значения")
	}
	if state == verifier {
		t.Error("state совпал с проверочным кодом")
	}

	otherState, _, _, err := NewSocialSecrets()
	if err != nil {
		t.Fatalf("генерация: %v", err)
	}
	if state == otherState {
		t.Error("два state совпали: источник случайности не работает")
	}
}

func TestSocialExchangeAndProfile(t *testing.T) {
	ctx := context.Background()
	var tokenForm url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if err := r.ParseForm(); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			tokenForm = r.PostForm
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "token-1"})
		case "/userinfo":
			if r.Header.Get("Authorization") != "Bearer token-1" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 4242, "email": "User@Example.COM", "name": "Пётр",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	provider := testProvider(t, server)
	client := NewSocialClient()

	token, err := client.Exchange(ctx, provider, "code-1", "verifier-1", "https://wish.example/callback")
	if err != nil {
		t.Fatalf("обмен кода: %v", err)
	}
	if token != "token-1" {
		t.Errorf("токен %q", token)
	}
	// Проверочный код PKCE обязателен: без него перехваченный код
	// обменивается на токен кем угодно.
	if tokenForm.Get("code_verifier") != "verifier-1" {
		t.Errorf("проверочный код не отправлен: %v", tokenForm)
	}
	if tokenForm.Get("client_secret") != "secret" {
		t.Error("секрет клиента не отправлен")
	}

	profile, err := client.Profile(ctx, provider, token)
	if err != nil {
		t.Fatalf("чтение профиля: %v", err)
	}
	// Идентификатор у одних провайдеров число, у других строка —
	// различать их незачем.
	if profile.ExternalId != "4242" {
		t.Errorf("идентификатор %q", profile.ExternalId)
	}
	if profile.Email != "user@example.com" {
		t.Errorf("почта %q: регистр должен приводиться", profile.Email)
	}
	if profile.Name != "Пётр" {
		t.Errorf("имя %q", profile.Name)
	}
}

func TestSocialProviderFailures(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"провайдер отвечает ошибкой", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}},
		{"ответ без токена", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		}},
		{"ответ не разбирается", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`не json`))
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()

			_, err := NewSocialClient().Exchange(ctx, testProvider(t, server),
				"code", "verifier", "https://wish.example/callback")
			if err == nil {
				t.Fatal("ошибка провайдера не замечена")
			}
			if !strings.Contains(err.Error(), "social provider") {
				t.Errorf("ошибка не отнесена к провайдеру: %v", err)
			}
		})
	}
}

func TestProfileWithoutIdentifier(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Без идентификатора связывать нечего: почта у провайдера
		// меняется и повторяется.
		_, _ = w.Write([]byte(`{"email":"user@example.com"}`))
	}))
	defer server.Close()

	if _, err := NewSocialClient().Profile(ctx, testProvider(t, server), "token"); err == nil {
		t.Error("профиль без идентификатора принят")
	}
}

func TestFieldPath(t *testing.T) {
	raw := map[string]any{
		"id": float64(7),
		"response": map[string]any{
			"email":    "nested@example.com",
			"verified": true,
		},
	}

	tests := []struct {
		path string
		want string
	}{
		{"id", "7"},
		{"response.email", "nested@example.com"},
		{"response.verified", "true"},
		{"missing", ""},
		{"response.missing", ""},
		{"id.deeper", ""},
		{"", ""},
	}

	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			if got := field(raw, test.path); got != test.want {
				t.Errorf("field(%q) = %q, ожидалось %q", test.path, got, test.want)
			}
		})
	}
}

func TestLoadSocialProviders(t *testing.T) {
	t.Setenv("SOCIAL_DEMO_CLIENT_ID", "client")
	t.Setenv("SOCIAL_DEMO_CLIENT_SECRET", "secret")
	t.Setenv("SOCIAL_DEMO_AUTH_URL", "https://demo.invalid/authorize")
	t.Setenv("SOCIAL_DEMO_TOKEN_URL", "https://demo.invalid/token")
	t.Setenv("SOCIAL_DEMO_USERINFO_URL", "https://demo.invalid/userinfo")
	t.Setenv("SOCIAL_DEMO_EMAIL_FIELD", "response.email")

	providers, err := LoadSocialProviders([]string{"demo", ""})
	if err != nil {
		t.Fatalf("разбор провайдеров: %v", err)
	}
	if len(providers) != 1 {
		t.Fatalf("провайдеров %d, ожидался один", len(providers))
	}

	demo := providers["demo"]
	if demo.ClientId != "client" || demo.EmailField != "response.email" {
		t.Errorf("провайдер разобран неверно: %+v", demo)
	}
	// Пути по умолчанию: у большинства провайдеров поля называются так.
	if demo.IdField != "id" || demo.NameField != "name" {
		t.Errorf("значения по умолчанию: %+v", demo)
	}

	t.Run("неполный провайдер роняет старт", func(t *testing.T) {
		t.Setenv("SOCIAL_BROKEN_CLIENT_ID", "client")
		if _, err := LoadSocialProviders([]string{"broken"}); err == nil {
			t.Error("провайдер без обязательных полей принят")
		}
	})
}
