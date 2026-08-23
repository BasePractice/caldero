//go:build integration

package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"wish/services"
	"wish/services/testsupport"

	"github.com/google/uuid"
)

const (
	adminToken       = "admin-token-for-tests"
	testClientId     = "web"
	testClientSecret = "client-secret"
	redirectURI      = "https://client.example/callback"
	// verifier — секрет PKCE клиента. Метод plain сервис не принимает
	// намеренно: перехваченный код с ним обменивается кем угодно.
	verifier = "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJ"
)

// challenge считает проверочное значение PKCE тем же способом, что и клиент.
func challenge() string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// newOAuth2Service поднимает сервис с настоящей базой и административным
// токеном: часть эндпоинтов доступна только по нему.
func newOAuth2Service(t *testing.T) (*Service, http.Handler) {
	t.Helper()

	cfg := testsupport.Prepare(t, "users")
	cfg.OAuth2GlobalSecret = "0123456789abcdef0123456789abcdef"
	cfg.AdminToken = adminToken
	cfg.KeyRotationMinInterval = time.Hour
	cfg.OAuth2Issuer = "https://wish.example/api/v1"

	service, err := newService(context.Background(), cfg)
	if err != nil {
		t.Fatalf("не удалось создать сервис: %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return service, registerHttpHandlers(service)
}

func form(handler http.Handler, method, path string, values url.Values, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// createClient заводит клиента с новым идентификатором: часть клиентов
// приходит из миграций, и фиксированное имя столкнулось бы с ними.
func createClient(t *testing.T, handler http.Handler) string {
	t.Helper()
	clientId := "client-" + uuid.NewString()[:8]
	recorder := form(handler, http.MethodPost, "/clients", url.Values{
		"client-id":     {clientId},
		"client-secret": {testClientSecret},
		"redirect-uri":  {redirectURI},
		// offline нужен, чтобы выдавался refresh-токен: без него
		// обновление доступа не проверить.
		"scopes": {"openid,read,write,offline"},
	}, map[string]string{"Authorization": "Bearer " + adminToken})

	if recorder.Code != http.StatusCreated {
		t.Fatalf("создание клиента: %d (%s)", recorder.Code, recorder.Body)
	}
	return clientId
}

func registerViaAPI(t *testing.T, handler http.Handler, username, phone string) uuid.UUID {
	t.Helper()
	recorder := form(handler, http.MethodPost, "/register", url.Values{
		"username":     {username},
		"password":     {"correct horse battery staple"},
		"phone":        {phone},
		"email":        {username + "@example.com"},
		"display_name": {"Пользователь"},
		"gender":       {"MALE"},
	}, nil)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("регистрация: %d (%s)", recorder.Code, recorder.Body)
	}
	id, err := uuid.Parse(recorder.Header().Get("X-User-Id"))
	if err != nil {
		t.Fatalf("идентификатор пользователя не возвращён: %v", err)
	}
	return id
}

// TestAuthorizationCodeFlow проходит контур целиком: клиент, регистрация,
// форма входа, код, обмен на токен и защищённый эндпоинт. Именно так им
// пользуется веб-интерфейс, и любая из этих ступеней ломает вход целиком.
func TestAuthorizationCodeFlow(t *testing.T) {
	_, handler := newOAuth2Service(t)
	clientId := createClient(t, handler)
	username := "user-" + uuid.NewString()[:8]
	userId := registerViaAPI(t, handler, username, "+79001110011")

	query := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientId},
		"redirect_uri":          {redirectURI},
		"scope":                 {"openid read offline"},
		"state":                 {"state-0123456789"},
		"code_challenge":        {challenge()},
		"code_challenge_method": {"S256"},
	}.Encode()

	t.Run("GET отдаёт форму входа", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/auth?"+query, nil))

		if recorder.Code != http.StatusOK {
			t.Fatalf("код ответа %d (%s)", recorder.Code, recorder.Body)
		}
		if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control %q: форма входа не должна кэшироваться", got)
		}
		if !strings.Contains(recorder.Body.String(), clientId) {
			t.Error("на форме не указано, какое приложение запрашивает доступ")
		}
	})

	t.Run("неверный пароль не различим с неизвестным пользователем", func(t *testing.T) {
		recorder := form(handler, http.MethodPost, "/auth?"+query, url.Values{
			"username": {username}, "password": {"wrong"},
		}, nil)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("код ответа %d (%s)", recorder.Code, recorder.Body)
		}
		wrongUser := form(handler, http.MethodPost, "/auth?"+query, url.Values{
			"username": {"нет-такого"}, "password": {"correct horse battery staple"},
		}, nil)
		if wrongUser.Code != recorder.Code {
			t.Errorf("коды ответа различаются: %d и %d", recorder.Code, wrongUser.Code)
		}
		if wrongUser.Body.String() != recorder.Body.String() {
			t.Error("тексты ответа различаются: логины можно перебирать")
		}
	})

	// Форма отправлена — это и есть согласие, и выдаётся код.
	recorder := form(handler, http.MethodPost, "/auth?"+query, url.Values{
		"username": {username}, "password": {"correct horse battery staple"},
	}, nil)
	if recorder.Code != http.StatusFound && recorder.Code != http.StatusSeeOther {
		t.Fatalf("код ответа %d (%s)", recorder.Code, recorder.Body)
	}
	location, err := url.Parse(recorder.Header().Get("Location"))
	if err != nil {
		t.Fatalf("разбор Location: %v", err)
	}
	code := location.Query().Get("code")
	if code == "" {
		t.Fatalf("код авторизации не выдан: %s", location)
	}
	if got := location.Query().Get("state"); got != "state-0123456789" {
		t.Errorf("state %q не вернулся клиенту", got)
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

	t.Run("защищённый эндпоинт отдаёт владельца токена", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/me", nil)
		request.Header.Set("Authorization", "Bearer "+token.AccessToken)
		handler.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusOK {
			t.Fatalf("код ответа %d (%s)", recorder.Code, recorder.Body)
		}
		var info userInfo
		if err := json.Unmarshal(recorder.Body.Bytes(), &info); err != nil {
			t.Fatalf("разбор ответа: %v", err)
		}
		if info.Subject != userId.String() {
			t.Errorf("subject %q, ожидался %s", info.Subject, userId)
		}
	})

	t.Run("профиль владельца токена", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/profile", nil)
		request.Header.Set("Authorization", "Bearer "+token.AccessToken)
		handler.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusOK {
			t.Fatalf("код ответа %d (%s)", recorder.Code, recorder.Body)
		}
		var profile map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &profile); err != nil {
			t.Fatalf("разбор ответа: %v", err)
		}
		if profile["username"] != username {
			t.Errorf("профиль %v", profile)
		}
	})

	t.Run("профиль меняется владельцем токена", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPatch, "/profile",
			strings.NewReader(`{"display_name":"Новое имя","gender":"FEMALE"}`))
		request.Header.Set("Authorization", "Bearer "+token.AccessToken)
		handler.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusOK {
			t.Fatalf("код ответа %d (%s)", recorder.Code, recorder.Body)
		}
		if !strings.Contains(recorder.Body.String(), "Новое имя") {
			t.Errorf("профиль не обновлён: %s", recorder.Body)
		}
	})

	t.Run("обновление токена", func(t *testing.T) {
		if token.RefreshToken == "" {
			t.Fatal("refresh-токен не выдан")
		}
		refreshed := exchange(t, handler, clientId, url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {token.RefreshToken},
		})
		if refreshed.AccessToken == "" {
			t.Error("обновлённый токен не выдан")
		}
	})

	t.Run("отзыв токена", func(t *testing.T) {
		recorder := form(handler, http.MethodPost, "/revoke", url.Values{
			"token":           {token.AccessToken},
			"token_type_hint": {"access_token"},
		}, map[string]string{"Authorization": basicAuth(clientId)})
		// RFC 7009: неизвестный токен — тоже 200, и это не ошибка.
		if recorder.Code != http.StatusOK {
			t.Fatalf("код ответа %d (%s)", recorder.Code, recorder.Body)
		}

		protectedRecorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/me", nil)
		request.Header.Set("Authorization", "Bearer "+token.AccessToken)
		handler.ServeHTTP(protectedRecorder, request)
		if protectedRecorder.Code != http.StatusUnauthorized {
			t.Errorf("отозванный токен принят: %d", protectedRecorder.Code)
		}
	})

	t.Run("повторный обмен того же кода отклоняется", func(t *testing.T) {
		// Использованный код означает перехват: fosite отзывает выданные
		// по нему токены, поэтому проверка идёт последней — иначе она
		// обесценила бы токены остальных подтестов.
		recorder := form(handler, http.MethodPost, "/token", url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {code},
			"redirect_uri":  {redirectURI},
			"code_verifier": {verifier},
		}, map[string]string{"Authorization": basicAuth(clientId)})
		if recorder.Code == http.StatusOK {
			t.Error("код обменян второй раз")
		}
	})

}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
}

func exchange(t *testing.T, handler http.Handler, clientId string, values url.Values) tokenResponse {
	t.Helper()
	recorder := form(handler, http.MethodPost, "/token", values,
		map[string]string{"Authorization": basicAuth(clientId)})

	if recorder.Code != http.StatusOK {
		t.Fatalf("обмен на токен: %d (%s)", recorder.Code, recorder.Body)
	}
	var token tokenResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &token); err != nil {
		t.Fatalf("разбор ответа: %v", err)
	}
	return token
}

func basicAuth(clientId string) string {
	request := httptest.NewRequest(http.MethodPost, "/token", nil)
	request.SetBasicAuth(clientId, testClientSecret)
	return request.Header.Get("Authorization")
}

// TestProtectedWithoutToken: защищённые эндпоинты обязаны отвечать 401,
// а не пускать без токена и не падать.
func TestProtectedWithoutToken(t *testing.T) {
	_, handler := newOAuth2Service(t)

	for _, path := range []string{"/me", "/profile", "/profile/identities"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
			if recorder.Code != http.StatusUnauthorized {
				t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusUnauthorized)
			}
		})
	}

	t.Run("испорченный токен", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/me", nil)
		request.Header.Set("Authorization", "Bearer не-токен")
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusUnauthorized)
		}
	})
}

func TestRegisterValidation(t *testing.T) {
	_, handler := newOAuth2Service(t)
	username := "user-" + uuid.NewString()[:8]

	tests := []struct {
		name   string
		values url.Values
		want   int
	}{
		{
			name:   "без имени и пароля",
			values: url.Values{"phone": {"+79001110022"}},
			want:   http.StatusBadRequest,
		},
		{
			// Телефон обязателен на уровне схемы: регистрация без него
			// не должна доходить до базы.
			name:   "без телефона",
			values: url.Values{"username": {username}, "password": {"пароль"}},
			want:   http.StatusBadRequest,
		},
		{
			name: "неразбираемый телефон",
			values: url.Values{
				"username": {username}, "password": {"пароль"}, "phone": {"тел"},
			},
			want: http.StatusBadRequest,
		},
		{
			name: "некорректная почта",
			values: url.Values{
				"username": {username}, "password": {"пароль"},
				"phone": {"+79001110033"}, "email": {"не-почта"},
			},
			want: http.StatusBadRequest,
		},
		{
			name: "неизвестный пол",
			values: url.Values{
				"username": {username}, "password": {"пароль"},
				"phone": {"+79001110044"}, "gender": {"WHATEVER"},
			},
			want: http.StatusBadRequest,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := form(handler, http.MethodPost, "/register", test.values, nil)
			if recorder.Code != test.want {
				t.Errorf("код ответа %d, ожидался %d (%s)", recorder.Code, test.want, recorder.Body)
			}
		})
	}

	t.Run("занятое имя не раскрывается", func(t *testing.T) {
		taken := url.Values{
			"username": {username}, "password": {"пароль"}, "phone": {"+79001110055"},
		}
		if recorder := form(handler, http.MethodPost, "/register", taken, nil); recorder.Code != http.StatusCreated {
			t.Fatalf("первая регистрация: %d (%s)", recorder.Code, recorder.Body)
		}
		recorder := form(handler, http.MethodPost, "/register", taken, nil)
		if recorder.Code != http.StatusConflict {
			t.Fatalf("код ответа %d, ожидался %d", recorder.Code, http.StatusConflict)
		}
		// Что именно занято, наружу не сообщается: иначе можно проверять,
		// зарегистрирован ли человек с известным номером.
		body := recorder.Body.String()
		if strings.Contains(body, username) || strings.Contains(body, "+79001110055") {
			t.Errorf("ответ раскрывает занятое значение: %s", body)
		}
	})
}

func TestAdminEndpoints(t *testing.T) {
	_, handler := newOAuth2Service(t)

	t.Run("создание клиента без токена", func(t *testing.T) {
		recorder := form(handler, http.MethodPost, "/clients", url.Values{
			"client-id": {"x"}, "client-secret": {"y"}, "redirect-uri": {redirectURI},
		}, nil)
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusUnauthorized)
		}
	})

	t.Run("создание клиента без обязательных полей", func(t *testing.T) {
		recorder := form(handler, http.MethodPost, "/clients", url.Values{"client-id": {"x"}},
			map[string]string{"Authorization": "Bearer " + adminToken})
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusBadRequest)
		}
	})

	t.Run("повторное создание клиента", func(t *testing.T) {
		clientId := createClient(t, handler)
		recorder := form(handler, http.MethodPost, "/clients", url.Values{
			"client-id": {clientId}, "client-secret": {testClientSecret}, "redirect-uri": {redirectURI},
		}, map[string]string{"Authorization": "Bearer " + adminToken})
		if recorder.Code != http.StatusConflict {
			t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusConflict)
		}
	})

	t.Run("ротация ключей ограничена по частоте", func(t *testing.T) {
		first := form(handler, http.MethodPost, "/rotate-keys", nil,
			map[string]string{"Authorization": "Bearer " + adminToken})
		if first.Code != http.StatusOK {
			t.Fatalf("ротация: %d (%s)", first.Code, first.Body)
		}
		// Ротация обесценивает выданные токены, поэтому её частота
		// ограничена даже для владельца административного токена.
		second := form(handler, http.MethodPost, "/rotate-keys", nil,
			map[string]string{"Authorization": "Bearer " + adminToken})
		if second.Code != http.StatusTooManyRequests {
			t.Errorf("код ответа %d, ожидался %d", second.Code, http.StatusTooManyRequests)
		}
		if second.Header().Get("Retry-After") == "" {
			t.Error("не указано, когда можно повторить")
		}
	})

	t.Run("ротация без токена", func(t *testing.T) {
		recorder := form(handler, http.MethodPost, "/rotate-keys", nil, nil)
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusUnauthorized)
		}
	})
}

// TestJWKS: шлюз проверяет подпись токена по этому набору ключей, и без
// kid он не знает, каким именно.
func TestJWKS(t *testing.T) {
	_, handler := newOAuth2Service(t)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("код ответа %d (%s)", recorder.Code, recorder.Body)
	}
	var jwks struct {
		Keys []struct {
			Kid string `json:"kid"`
			Alg string `json:"alg"`
			Use string `json:"use"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &jwks); err != nil {
		t.Fatalf("разбор ответа: %v", err)
	}
	if len(jwks.Keys) == 0 {
		t.Fatal("набор ключей пуст")
	}
	for _, key := range jwks.Keys {
		if key.Kid == "" || key.Alg != "RS256" || key.Use != "sig" {
			t.Errorf("ключ %+v", key)
		}
	}
}

func TestPublicProfileAndContacts(t *testing.T) {
	service, handler := newOAuth2Service(t)
	username := "user-" + uuid.NewString()[:8]
	userId := registerViaAPI(t, handler, username, "+79001110066")
	reader := uuid.New()

	get := func(path string, user uuid.UUID, roles ...string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		if user != uuid.Nil {
			request.Header.Set("X-Authorized-Id", user.String())
			if len(roles) > 0 {
				request.Header.Set("X-Roles", strings.Join(roles, ","))
			}
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	t.Run("карточка требует аутентификации", func(t *testing.T) {
		// Иначе идентификаторы, попадающие в ссылки и заголовки,
		// превращаются в способ обойти систему целиком.
		if code := get("/users/"+userId.String(), uuid.Nil).Code; code != http.StatusUnauthorized {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusUnauthorized)
		}
	})

	t.Run("карточка видна любому аутентифицированному", func(t *testing.T) {
		recorder := get("/users/"+userId.String(), reader)
		if recorder.Code != http.StatusOK {
			t.Fatalf("код ответа %d (%s)", recorder.Code, recorder.Body)
		}
		// В публичной карточке контактов быть не должно.
		if strings.Contains(recorder.Body.String(), "+79001110066") {
			t.Errorf("телефон попал в публичную карточку: %s", recorder.Body)
		}
	})

	t.Run("карточка несуществующего пользователя", func(t *testing.T) {
		if code := get("/users/"+uuid.NewString(), reader).Code; code != http.StatusNotFound {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusNotFound)
		}
	})

	t.Run("неразбираемый идентификатор", func(t *testing.T) {
		if code := get("/users/не-uuid", reader).Code; code != http.StatusBadRequest {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusBadRequest)
		}
	})

	t.Run("контакты без роли оператора запрещены", func(t *testing.T) {
		if code := get("/users/"+userId.String()+"/contacts", reader).Code; code != http.StatusForbidden {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusForbidden)
		}
	})

	t.Run("контакты оператору", func(t *testing.T) {
		recorder := get("/users/"+userId.String()+"/contacts", reader, services.RoleOperator)
		if recorder.Code != http.StatusOK {
			t.Fatalf("код ответа %d (%s)", recorder.Code, recorder.Body)
		}
		var contacts map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &contacts); err != nil {
			t.Fatalf("разбор ответа: %v", err)
		}
		if contacts["phone"] != "+79001110066" {
			t.Errorf("контакты %v", contacts)
		}
		// Обязательность формы — ещё не подтверждение: пока номер
		// не подтверждён, полагаться на него нельзя.
		if contacts["phone_confirmed"] != false {
			t.Errorf("новый номер отмечен подтверждённым: %v", contacts)
		}
	})

	t.Run("контакты без аутентификации", func(t *testing.T) {
		if code := get("/users/"+userId.String()+"/contacts", uuid.Nil).Code; code != http.StatusUnauthorized {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusUnauthorized)
		}
	})

	t.Run("контакты по неразбираемому идентификатору", func(t *testing.T) {
		code := get("/users/не-uuid/contacts", reader, services.RoleOperator).Code
		if code != http.StatusBadRequest {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusBadRequest)
		}
	})

	t.Run("контакты несуществующего пользователя", func(t *testing.T) {
		code := get("/users/"+uuid.NewString()+"/contacts", reader, services.RoleOperator).Code
		if code != http.StatusNotFound {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusNotFound)
		}
	})

	t.Run("служебные задачи по расписанию", func(t *testing.T) {
		ctx := context.Background()
		if err := service.CleanupExpiredTokens(ctx); err != nil {
			t.Errorf("очистка токенов: %v", err)
		}
		if err := service.CleanupSocialLogins(ctx); err != nil {
			t.Errorf("очистка состояний входа: %v", err)
		}
		if err := service.Ping(ctx); err != nil {
			t.Errorf("проба готовности: %v", err)
		}
		if service.Stats().Stats().MaxOpenConnections == 0 {
			t.Error("статистика пула не заполнена")
		}
	})
}
