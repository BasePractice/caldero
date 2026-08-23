//go:build integration

package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"testing"
	"time"

	"wish/services/testsupport"

	"github.com/google/uuid"
	"github.com/ory/fosite"
	"github.com/ory/fosite/handler/oauth2"
	"github.com/ory/fosite/token/jwt"
	"golang.org/x/crypto/bcrypt"
)

func TestUsersRepository(t *testing.T) {
	ctx := context.Background()
	cfg := testsupport.Prepare(t, "users")
	cfg.KeyMasterKey = "мастер-ключ-интеграционного-теста"

	db, err := NewDatabaseUsers(ctx, cfg)
	if err != nil {
		t.Fatalf("не удалось открыть репозиторий: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	t.Run("ключи подписи читаются обратно", func(t *testing.T) {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("генерация ключа: %v", err)
		}
		encoded := x509.MarshalPKCS1PrivateKey(key)

		// Запрос читал keys по колонке id, которой в схеме нет, — поэтому
		// подпись токенов не работала вообще.
		keyId, err := db.CreateKey(ctx, encoded)
		if err != nil {
			t.Fatalf("сохранение ключа: %v", err)
		}
		loaded, err := db.GetKey(ctx, keyId)
		if err != nil {
			t.Fatalf("чтение ключа: %v", err)
		}
		if _, err = x509.ParsePKCS1PrivateKey(loaded); err != nil {
			t.Fatalf("прочитанный ключ не разбирается: %v", err)
		}

		last, err := db.GetLastKey(ctx)
		if err != nil {
			t.Fatalf("чтение последнего ключа: %v", err)
		}
		if last != keyId {
			t.Errorf("последний ключ %s, ожидался %s", last, keyId)
		}
	})

	t.Run("ротация оставляет не больше двух ключей", func(t *testing.T) {
		for range 3 {
			key, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatalf("генерация ключа: %v", err)
			}
			if _, err = db.CreateKey(ctx, x509.MarshalPKCS1PrivateKey(key)); err != nil {
				t.Fatalf("сохранение ключа: %v", err)
			}
		}
		// ClearKeys использовал rowid — псевдоколонку SQLite, и падал всегда.
		if err := db.ClearKeys(ctx); err != nil {
			t.Fatalf("очистка ключей: %v", err)
		}

		var keys []Key
		if err := db.GetKeys(ctx, func(id string, data []byte) {
			keys = append(keys, Key{Id: id, Data: data})
		}); err != nil {
			t.Fatalf("чтение набора ключей: %v", err)
		}
		if len(keys) != 2 {
			t.Fatalf("ключей %d, ожидалось 2", len(keys))
		}
		for _, key := range keys {
			if _, err := x509.ParsePKCS1PrivateKey(key.Data); err != nil {
				t.Fatalf("ключ %s не разбирается: %v", key.Id, err)
			}
		}
	})

	t.Run("аутентификация пользователя", func(t *testing.T) {
		hash, err := bcrypt.GenerateFromPassword([]byte("correct-horse-battery"), bcrypt.MinCost)
		if err != nil {
			t.Fatalf("хеширование: %v", err)
		}
		user, err := db.CreateUser(ctx, "integration-user", string(hash))
		if err != nil {
			t.Fatalf("создание пользователя: %v", err)
		}

		subject, err := db.Authenticate(ctx, "integration-user", "correct-horse-battery")
		if err != nil {
			t.Fatalf("аутентификация: %v", err)
		}
		if subject != user.Id.String() {
			t.Errorf("subject = %s, ожидался %s", subject, user.Id)
		}

		// Неизвестный пользователь и неверный пароль должны быть неразличимы.
		if _, err = db.Authenticate(ctx, "integration-user", "wrong"); !errors.Is(err, fosite.ErrNotFound) {
			t.Errorf("неверный пароль дал %v, ожидалась ErrNotFound", err)
		}
		if _, err = db.Authenticate(ctx, "нет-такого", "wrong"); !errors.Is(err, fosite.ErrNotFound) {
			t.Errorf("неизвестный пользователь дал %v, ожидалась ErrNotFound", err)
		}

		if _, err = db.CreateUser(ctx, "integration-user", string(hash)); !db.IsUniqueConstraintError(err) {
			t.Errorf("повторное имя дало %v, ожидалось нарушение уникальности", err)
		}
	})

	t.Run("роли пользователя", func(t *testing.T) {
		hash, err := bcrypt.GenerateFromPassword([]byte("p"), bcrypt.MinCost)
		if err != nil {
			t.Fatalf("хеширование: %v", err)
		}
		user, err := db.CreateUser(ctx, "role-user", string(hash))
		if err != nil {
			t.Fatalf("создание пользователя: %v", err)
		}

		roles, err := db.GetUserRoles(ctx, user.Id)
		if err != nil {
			t.Fatalf("чтение ролей: %v", err)
		}
		if len(roles) != 0 {
			t.Errorf("роли = %v, ожидался пустой список", roles)
		}
	})

	t.Run("сессия токена сохраняется и читается", func(t *testing.T) {
		client, err := db.GetClient(ctx, "test-client")
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

		// Раньше здесь сохранялся сериализованный fosite.Request, который
		// не разбирался обратно: поля Client и Session — интерфейсы.
		if err = db.CreateAccessTokenSession(ctx, "signature-1", request); err != nil {
			t.Fatalf("сохранение сессии: %v", err)
		}

		restored, err := db.GetAccessTokenSession(ctx, "signature-1", new(jwtSession))
		if err != nil {
			t.Fatalf("чтение сессии: %v", err)
		}
		if restored.GetID() != request.ID {
			t.Errorf("id = %s, ожидался %s", restored.GetID(), request.ID)
		}
		if restored.GetClient().GetID() != "test-client" {
			t.Errorf("клиент = %s, ожидался test-client", restored.GetClient().GetID())
		}
		if !restored.GetGrantedScopes().Has("read") {
			t.Errorf("granted scope = %v, ожидался read", restored.GetGrantedScopes())
		}
		if restored.GetSession().GetSubject() != session.GetSubject() {
			t.Errorf("subject = %s, ожидался %s", restored.GetSession().GetSubject(), session.GetSubject())
		}

		if err = db.DeleteAccessTokenSession(ctx, "signature-1"); err != nil {
			t.Fatalf("удаление сессии: %v", err)
		}
		if _, err = db.GetAccessTokenSession(ctx, "signature-1", new(jwtSession)); err == nil {
			t.Error("удалённая сессия всё ещё читается")
		}
	})

	t.Run("просроченные токены удаляются", func(t *testing.T) {
		store := db.(*ds)
		if _, err := store.db.ExecContext(ctx,
			`INSERT INTO oauth_tokens (signature, request_id, session_data, expires_at, token_type)
			 VALUES ('expired', 'req', '{}', now() - INTERVAL '1 hour', 'access'),
			        ('valid', 'req', '{}', now() + INTERVAL '1 hour', 'access')`); err != nil {
			t.Fatalf("подготовка: %v", err)
		}

		deleted, err := db.DeleteExpiredTokens(ctx)
		if err != nil {
			t.Fatalf("очистка: %v", err)
		}
		if deleted != 1 {
			t.Errorf("удалено %d записей, ожидалась 1", deleted)
		}
	})
}
