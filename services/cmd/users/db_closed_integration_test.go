//go:build integration

package main

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"wish/services/testsupport"

	"github.com/google/uuid"
)

// TestRepositoryReportsBrokenDatabase проверяет свойство, которое иначе
// не проверяется ничем: каждый метод репозитория сообщает о сбое базы,
// а не возвращает пустой результат с nil-ошибкой.
//
// Здесь это вопрос доступа: молчаливо пустой список ролей или ненайденная
// сессия токена выглядят как обычный отказ в правах, и настоящая причина
// теряется.
func TestRepositoryReportsBrokenDatabase(t *testing.T) {
	ctx := context.Background()
	db, err := NewDatabaseUsers(ctx, testsupport.Prepare(t, "users"))
	if err != nil {
		t.Fatalf("не удалось открыть репозиторий: %v", err)
	}
	// База закрывается намеренно: дальше любой запрос обязан падать.
	if err := db.Close(); err != nil {
		t.Fatalf("закрытие репозитория: %v", err)
	}

	user := uuid.New()

	calls := map[string]func() error{
		"CreateUser": func() error {
			_, err := db.CreateUser(ctx, Registration{
				Username: "user", PasswordHash: "hash", Phone: "+79001112233",
			})
			return err
		},
		"GetUser": func() error {
			_, err := db.GetUser(ctx, "user")
			return err
		},
		"GetUserById": func() error {
			_, err := db.GetUserById(ctx, user)
			return err
		},
		"UpdateProfile": func() error {
			_, err := db.UpdateProfile(ctx, user, ProfileUpdate{})
			return err
		},
		"Authenticate": func() error {
			_, err := db.Authenticate(ctx, "user", "secret")
			return err
		},
		"GetUserRoles": func() error {
			_, err := db.GetUserRoles(ctx, user)
			return err
		},
		"CreateClient": func() error {
			return db.CreateClient(ctx, "client", "secret", "https://example", "openid", "code", "authorization_code")
		},
		"GetClient": func() error {
			_, err := db.GetClient(ctx, "client")
			return err
		},
		"DeleteExpiredTokens": func() error {
			_, err := db.DeleteExpiredTokens(ctx)
			return err
		},
		"CreateConfirmation": func() error {
			_, err := db.CreateConfirmation(ctx, Confirmation{
				UserId: user, Kind: ConfirmPhone, Target: "+79001112233",
				CodeHash: []byte("hash"), ExpiresAt: time.Now().Add(time.Minute),
			})
			return err
		},
		"ActiveConfirmation": func() error {
			_, err := db.ActiveConfirmation(ctx, user, ConfirmPhone)
			return err
		},
		"CountConfirmations": func() error {
			_, _, err := db.CountConfirmations(ctx, user, ConfirmPhone, time.Hour)
			return err
		},
		"FailConfirmation": func() error {
			return db.FailConfirmation(ctx, uuid.New())
		},
		"ConfirmContact": func() error {
			return db.ConfirmContact(ctx, uuid.New(), user, ConfirmPhone, "+79001112233")
		},
		"StartSocialLogin": func() error {
			return db.StartSocialLogin(ctx, SocialLogin{
				State: "state", Provider: "demo", Verifier: "verifier",
				ExpiresAt: time.Now().Add(time.Minute),
			})
		},
		"TakeSocialLogin": func() error {
			_, err := db.TakeSocialLogin(ctx, "state")
			return err
		},
		"DeleteExpiredSocialLogins": func() error {
			_, err := db.DeleteExpiredSocialLogins(ctx)
			return err
		},
		"IdentityUser": func() error {
			_, err := db.IdentityUser(ctx, "demo", "ext-1")
			return err
		},
		"LinkIdentity": func() error {
			return db.LinkIdentity(ctx, user, SocialProfile{Provider: "demo", ExternalId: "ext-1"})
		},
		"Identities": func() error {
			_, err := db.Identities(ctx, user)
			return err
		},
		"UnlinkIdentity": func() error {
			return db.UnlinkIdentity(ctx, user, "demo")
		},
		"CreateSocialUser": func() error {
			_, err := db.CreateSocialUser(ctx, SocialProfile{Provider: "demo", ExternalId: "ext-1"})
			return err
		},
		"GetLastKey": func() error {
			_, err := db.GetLastKey(ctx)
			return err
		},
		"GetKey": func() error {
			_, err := db.GetKey(ctx, "kid")
			return err
		},
		"GetKeys": func() error {
			return db.GetKeys(ctx, func(string, []byte) {})
		},
		"CreateKey": func() error {
			_, err := db.CreateKey(ctx, []byte("key"))
			return err
		},
		"ClearKeys": func() error {
			return db.ClearKeys(ctx)
		},
		"Ping": func() error {
			return db.Ping(ctx)
		},
	}

	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil {
				t.Error("сбой базы не превратился в ошибку")
			}
		})
	}

	if db.Stats().MaxOpenConnections == 0 {
		t.Error("статистика пула не заполнена")
	}
}

// TestHandlersReportBrokenDatabase: недоступная база должна отвечать
// пятисоткой, а не выглядеть отказом в регистрации — иначе причина
// теряется, и пользователь пробует снова с теми же данными.
func TestHandlersReportBrokenDatabase(t *testing.T) {
	cfg := testsupport.Prepare(t, "users")
	cfg.OAuth2GlobalSecret = "0123456789abcdef0123456789abcdef"
	cfg.AdminToken = adminToken

	service, err := newService(context.Background(), cfg)
	if err != nil {
		t.Fatalf("не удалось создать сервис: %v", err)
	}
	handler := registerHttpHandlers(service)
	// База закрывается уже после старта: ключи подписи успели загрузиться,
	// а запросы к базе — нет.
	if err := service.Close(); err != nil {
		t.Fatalf("закрытие репозитория: %v", err)
	}

	tests := []struct {
		name    string
		path    string
		values  url.Values
		headers map[string]string
	}{
		{
			name: "регистрация",
			path: "/register",
			values: url.Values{
				"username": {"user-" + uuid.NewString()[:8]},
				"password": {"пароль"},
				"phone":    {"+79005550011"},
			},
		},
		{
			name: "создание клиента",
			path: "/clients",
			values: url.Values{
				"client-id":     {"client-" + uuid.NewString()[:8]},
				"client-secret": {"secret"},
				"redirect-uri":  {"https://client.example/callback"},
			},
			headers: map[string]string{"Authorization": "Bearer " + adminToken},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := form(handler, http.MethodPost, test.path, test.values, test.headers)
			if recorder.Code != http.StatusInternalServerError {
				t.Errorf("код ответа %d, ожидался %d (%s)",
					recorder.Code, http.StatusInternalServerError, recorder.Body)
			}
		})
	}

}
