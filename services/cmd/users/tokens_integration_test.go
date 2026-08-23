//go:build integration

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"wish/services/testsupport"

	"github.com/google/uuid"
	"github.com/ory/fosite"
	"github.com/ory/fosite/handler/oauth2"
	"github.com/ory/fosite/token/jwt"
)

// tokenRequest собирает запрос так, как его сохраняет fosite: клиент
// и сессия — интерфейсы, и восстанавливаются они по отдельности.
func tokenRequest(t *testing.T, db DatabaseUsers, clientId string) *fosite.Request {
	t.Helper()

	client, err := db.GetClient(context.Background(), clientId)
	if err != nil {
		t.Fatalf("чтение клиента: %v", err)
	}

	request := fosite.NewRequest()
	request.ID = uuid.NewString()
	request.Client = client
	request.RequestedScope = fosite.Arguments{"openid", "read"}
	request.GrantedScope = fosite.Arguments{"read"}
	session := &jwtSession{oauth2.JWTSession{
		Subject:   uuid.NewString(),
		JWTClaims: &jwt.JWTClaims{},
		JWTHeader: jwt.NewHeaders(),
	}}
	session.SetExpiresAt(fosite.AccessToken, request.RequestedAt.Add(time.Hour))
	request.Session = session
	return request
}

// testClient заводит клиента и возвращает его идентификатор.
func testClient(t *testing.T, db DatabaseUsers) string {
	t.Helper()
	clientId := "client-" + uuid.NewString()[:8]
	if err := db.CreateClient(context.Background(), clientId, "secret",
		"https://client.example/callback", "openid,read", "code",
		"authorization_code,refresh_token"); err != nil {
		t.Fatalf("создание клиента: %v", err)
	}
	return clientId
}

// TestRefreshAndPKCESessions закрывает хранилище сессий, до которого
// не доходит сквозной сценарий: refresh-поток и PKCE fosite вызывает сам,
// и их поведение видно только отсюда.
func TestRefreshAndPKCESessions(t *testing.T) {
	ctx := context.Background()
	db, err := NewDatabaseUsers(ctx, testsupport.Prepare(t, "users"))
	if err != nil {
		t.Fatalf("не удалось открыть репозиторий: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	clientId := testClient(t, db)

	t.Run("refresh-сессия сохраняется, читается и удаляется", func(t *testing.T) {
		request := tokenRequest(t, db, clientId)
		if err := db.CreateRefreshTokenSession(ctx, "refresh-1", "", request); err != nil {
			t.Fatalf("сохранение: %v", err)
		}
		restored, err := db.GetRefreshTokenSession(ctx, "refresh-1", new(jwtSession))
		if err != nil {
			t.Fatalf("чтение: %v", err)
		}
		if restored.GetID() != request.ID {
			t.Errorf("id = %s, ожидался %s", restored.GetID(), request.ID)
		}
		if err := db.DeleteRefreshTokenSession(ctx, "refresh-1"); err != nil {
			t.Fatalf("удаление: %v", err)
		}
		if _, err := db.GetRefreshTokenSession(ctx, "refresh-1", new(jwtSession)); err == nil {
			t.Error("удалённая сессия всё ещё читается")
		}
	})

	t.Run("PKCE-сессия живёт отдельно от токенов", func(t *testing.T) {
		request := tokenRequest(t, db, clientId)
		if err := db.CreatePKCERequestSession(ctx, "pkce-1", request); err != nil {
			t.Fatalf("сохранение: %v", err)
		}
		if _, err := db.GetPKCERequestSession(ctx, "pkce-1", new(jwtSession)); err != nil {
			t.Fatalf("чтение: %v", err)
		}
		if err := db.DeletePKCERequestSession(ctx, "pkce-1"); err != nil {
			t.Fatalf("удаление: %v", err)
		}
		if _, err := db.GetPKCERequestSession(ctx, "pkce-1", new(jwtSession)); err == nil {
			t.Error("удалённая сессия всё ещё читается")
		}
	})

	// Ротация вызывается refresh-потоком безусловно: старая пара
	// отзывается целиком, и обе записи должны исчезнуть.
	t.Run("ротация отзывает пару токенов", func(t *testing.T) {
		request := tokenRequest(t, db, clientId)
		if err := db.CreateAccessTokenSession(ctx, "access-rotate", request); err != nil {
			t.Fatalf("сохранение access: %v", err)
		}
		if err := db.CreateRefreshTokenSession(ctx, "refresh-rotate", "", request); err != nil {
			t.Fatalf("сохранение refresh: %v", err)
		}

		if err := db.RotateRefreshToken(ctx, request.ID, "refresh-rotate"); err != nil {
			t.Fatalf("ротация: %v", err)
		}
		if _, err := db.GetAccessTokenSession(ctx, "access-rotate", new(jwtSession)); err == nil {
			t.Error("access-токен пережил ротацию")
		}
		if _, err := db.GetRefreshTokenSession(ctx, "refresh-rotate", new(jwtSession)); err == nil {
			t.Error("refresh-токен пережил ротацию")
		}
	})

	// Перехваченный код отзывает всё, что по нему выдано: молчаливое
	// «не найдено» оставило бы перехват незамеченным.
	t.Run("использованный код отличается от несуществующего", func(t *testing.T) {
		request := tokenRequest(t, db, clientId)
		if err := db.CreateAuthorizeCodeSession(ctx, "code-1", request); err != nil {
			t.Fatalf("сохранение кода: %v", err)
		}
		if err := db.InvalidateAuthorizeCodeSession(ctx, "code-1"); err != nil {
			t.Fatalf("погашение кода: %v", err)
		}

		_, err := db.GetAuthorizeCodeSession(ctx, "code-1", new(jwtSession))
		if !errors.Is(err, fosite.ErrInvalidatedAuthorizeCode) {
			t.Errorf("получено %v, ожидалась ErrInvalidatedAuthorizeCode", err)
		}
		if _, err := db.GetAuthorizeCodeSession(ctx, "нет-такого", new(jwtSession)); err == nil {
			t.Error("несуществующий код прочитан")
		}
	})

	// Аутентификация клиента по JWT не поддерживается: методы обязаны
	// отвечать без ошибки, иначе fosite отклонит любой запрос.
	t.Run("проверка клиентских assertion-токенов не мешает выдаче", func(t *testing.T) {
		store := db.(*ds)
		if err := store.ClientAssertionJWTValid(ctx, "jti"); err != nil {
			t.Errorf("проверка jti: %v", err)
		}
		if err := store.SetClientAssertionJWT(ctx, "jti", time.Now().Add(time.Hour)); err != nil {
			t.Errorf("запись jti: %v", err)
		}
	})

	t.Run("сессия неизвестного клиента не восстанавливается", func(t *testing.T) {
		request := tokenRequest(t, db, clientId)
		if err := db.CreateAccessTokenSession(ctx, "access-orphan", request); err != nil {
			t.Fatalf("сохранение: %v", err)
		}
		store := db.(*ds)
		if _, err := store.db.ExecContext(ctx,
			"DELETE FROM oauth_clients WHERE client_id = $1", clientId); err != nil {
			t.Fatalf("удаление клиента: %v", err)
		}
		if _, err := db.GetAccessTokenSession(ctx, "access-orphan", new(jwtSession)); err == nil {
			t.Error("сессия восстановлена без клиента")
		}
	})
}
