//go:build integration

package main

import (
	"context"
	"crypto/rsa"
	"testing"

	"wish/services"
	"wish/services/testsupport"
)

// TestKeyManager проверяет ключи подписи: шлюз проверяет токен по JWKS,
// и без совпадения kid с ключом подпись проверить нечем.
func TestKeyManager(t *testing.T) {
	ctx := context.Background()
	db, err := NewDatabaseUsers(ctx, testsupport.Prepare(t, "users"))
	if err != nil {
		t.Fatalf("не удалось открыть репозиторий: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	manager, err := NewKeyManager(ctx, db)
	if err != nil {
		t.Fatalf("создание менеджера ключей: %v", err)
	}

	kid, err := manager.GetPublicKeyId(ctx)
	if err != nil {
		t.Fatalf("идентификатор ключа: %v", err)
	}
	if kid == "" {
		t.Fatal("ключ не создан при первом старте")
	}

	t.Run("публичный ключ читается по kid", func(t *testing.T) {
		key, err := manager.GetPublicKey(ctx, kid)
		if err != nil {
			t.Fatalf("чтение ключа: %v", err)
		}
		if _, ok := key.(*rsa.PublicKey); !ok {
			t.Errorf("получен ключ типа %T, ожидался RSA", key)
		}
	})

	t.Run("пустой kid означает текущий ключ", func(t *testing.T) {
		if _, err := manager.GetPublicKey(ctx, ""); err != nil {
			t.Errorf("чтение текущего ключа: %v", err)
		}
	})

	t.Run("неизвестный kid не читается", func(t *testing.T) {
		if _, err := manager.GetPublicKey(ctx, "нет-такого"); err == nil {
			t.Error("прочитан неизвестный ключ")
		}
	})

	t.Run("повторный старт берёт существующий ключ", func(t *testing.T) {
		// Иначе каждый перезапуск обесценивал бы выданные токены,
		// а два инстанса не проверяли бы токены друг друга.
		again, err := NewKeyManager(ctx, db)
		if err != nil {
			t.Fatalf("создание менеджера ключей: %v", err)
		}
		sameKid, err := again.GetPublicKeyId(ctx)
		if err != nil {
			t.Fatalf("идентификатор ключа: %v", err)
		}
		if sameKid != kid {
			t.Errorf("ключ %s, ожидался прежний %s", sameKid, kid)
		}
	})

	t.Run("ротация меняет текущий ключ", func(t *testing.T) {
		if err := manager.RotateKeys(ctx); err != nil {
			t.Fatalf("ротация: %v", err)
		}
		rotated, err := manager.GetPublicKeyId(ctx)
		if err != nil {
			t.Fatalf("идентификатор ключа: %v", err)
		}
		if rotated == kid {
			t.Error("ротация оставила прежний ключ")
		}

		keys, err := manager.GetKeys(ctx)
		if err != nil {
			t.Fatalf("чтение набора ключей: %v", err)
		}
		if len(keys) == 0 {
			t.Error("набор ключей пуст")
		}
	})
}

// TestKeyManagerWithBrokenDatabase: без ключа подписи сервис не выдаст
// ни одного токена, и старт обязан падать, а не поднимать сервис
// без возможности подписать.
func TestKeyManagerWithBrokenDatabase(t *testing.T) {
	ctx := context.Background()
	db, err := NewDatabaseUsers(ctx, testsupport.Prepare(t, "users"))
	if err != nil {
		t.Fatalf("не удалось открыть репозиторий: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("закрытие репозитория: %v", err)
	}

	if _, err := NewKeyManager(ctx, db); err == nil {
		t.Fatal("менеджер ключей создан при недоступной базе")
	}
}

// TestOAuth2Secret фиксирует требование к секрету подписи: ровно 32 байта.
// Раньше он генерировался случайно при каждом старте — токены не переживали
// рестарт, а два инстанса не могли проверить токены друг друга.
func TestOAuth2Secret(t *testing.T) {
	tests := []struct {
		name    string
		secret  string
		wantErr bool
	}{
		{"ровно 32 байта", "0123456789abcdef0123456789abcdef", false},
		{"пусто — случайный секрет", "", false},
		{"короче 32 байт", "слишком-короткий", true},
		{"длиннее 32 байт", "0123456789abcdef0123456789abcdef0", true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			secret, err := oauth2Secret(services.Config{OAuth2GlobalSecret: test.secret})
			if test.wantErr {
				if err == nil {
					t.Error("секрет принят, ожидался отказ")
				}
				return
			}
			if err != nil {
				t.Fatalf("секрет отклонён: %v", err)
			}
			if len(secret) != 32 {
				t.Errorf("длина секрета %d, ожидалось 32", len(secret))
			}
		})
	}
}
