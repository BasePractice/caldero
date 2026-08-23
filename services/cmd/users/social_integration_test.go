//go:build integration

package main

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"wish/services/testsupport"

	"github.com/google/uuid"
)

func newSocialService(t *testing.T) *Service {
	t.Helper()

	cfg := testsupport.Prepare(t, "users")
	cfg.OAuth2GlobalSecret = "0123456789abcdef0123456789abcdef"
	cfg.SocialRedirectBase = "https://wish.example/api/v1"

	service, err := newService(context.Background(), cfg)
	if err != nil {
		t.Fatalf("не удалось создать сервис: %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return service
}

func testProfile(external, email string) SocialProfile {
	return SocialProfile{
		Provider: "demo", ExternalId: external, Email: email, Name: "Пётр",
	}
}

// TestSocialLoginCreatesUser проверяет сквозной сценарий: первый вход
// через провайдера заводит локального пользователя.
func TestSocialLoginCreatesUser(t *testing.T) {
	ctx := context.Background()
	service := newSocialService(t)

	profile := testProfile("ext-1", "user@example.com")
	userId, err := service.resolveIdentity(ctx, SocialLogin{Provider: "demo"}, profile)
	if err != nil {
		t.Fatalf("первый вход: %v", err)
	}

	created, err := service.db.GetUserById(ctx, userId)
	if err != nil {
		t.Fatalf("чтение пользователя: %v", err)
	}
	// Пароля у такого пользователя нет: вход возможен только через
	// провайдера, пока он не задаст пароль сам.
	if created.PasswordSet {
		t.Error("пользователь создан с признаком заданного пароля")
	}
	// Почта провайдера в профиль не переносится: она может быть
	// не подтверждена у провайдера.
	if created.Email.Valid {
		t.Errorf("почта провайдера попала в профиль: %q", created.Email.String)
	}

	identities, err := service.db.Identities(ctx, userId)
	if err != nil {
		t.Fatalf("чтение идентичностей: %v", err)
	}
	if len(identities) != 1 || identities[0].Email != "user@example.com" {
		t.Errorf("идентичность: %+v", identities)
	}

	t.Run("повторный вход не создаёт второго пользователя", func(t *testing.T) {
		again, err := service.resolveIdentity(ctx, SocialLogin{Provider: "demo"}, profile)
		if err != nil {
			t.Fatalf("повторный вход: %v", err)
		}
		if again != userId {
			t.Errorf("вход дал другого пользователя: %s вместо %s", again, userId)
		}
	})
}

// TestSocialLoginDoesNotHijackByEmail закрывает главную ловушку внешнего
// входа: совпадение почты не должно отдавать чужой профиль.
func TestSocialLoginDoesNotHijackByEmail(t *testing.T) {
	ctx := context.Background()
	service := newSocialService(t)

	local, err := service.db.CreateUser(ctx, Registration{
		Username:     "local-" + uuid.NewString()[:8],
		PasswordHash: "not-a-real-hash",
		Phone:        "+79004445566",
		Email:        "victim@example.com",
	})
	if err != nil {
		t.Fatalf("регистрация: %v", err)
	}

	// У провайдера тот же адрес почты — и он может быть не подтверждён.
	userId, err := service.resolveIdentity(ctx, SocialLogin{Provider: "demo"},
		testProfile("ext-hijack", "victim@example.com"))
	if err != nil {
		t.Fatalf("вход через провайдера: %v", err)
	}
	if userId == local.Id {
		t.Fatal("вход по совпадению почты отдал чужой профиль")
	}
}

func TestSocialLinkToExistingUser(t *testing.T) {
	ctx := context.Background()
	service := newSocialService(t)

	local, err := service.db.CreateUser(ctx, Registration{
		Username:     "local-" + uuid.NewString()[:8],
		PasswordHash: "not-a-real-hash",
		Phone:        "+79004445577",
	})
	if err != nil {
		t.Fatalf("регистрация: %v", err)
	}

	userId, err := service.resolveIdentity(ctx,
		SocialLogin{Provider: "demo", LinkUserId: &local.Id},
		testProfile("ext-link", "link@example.com"))
	if err != nil {
		t.Fatalf("привязка: %v", err)
	}
	if userId != local.Id {
		t.Fatalf("привязка создала нового пользователя: %s", userId)
	}

	t.Run("занятую идентичность к другому не привязать", func(t *testing.T) {
		other, err := service.db.CreateUser(ctx, Registration{
			Username:     "other-" + uuid.NewString()[:8],
			PasswordHash: "not-a-real-hash",
			Phone:        "+79004445588",
		})
		if err != nil {
			t.Fatalf("регистрация: %v", err)
		}
		_, err = service.resolveIdentity(ctx,
			SocialLogin{Provider: "demo", LinkUserId: &other.Id},
			testProfile("ext-link", "link@example.com"))
		if !errors.Is(err, ErrIdentityTaken) {
			t.Errorf("получено %v, ожидалась %v", err, ErrIdentityTaken)
		}
	})
}

// TestSocialStateIsSingleUse проверяет, что перехваченный ответ провайдера
// нельзя предъявить повторно.
func TestSocialStateIsSingleUse(t *testing.T) {
	ctx := context.Background()
	service := newSocialService(t)

	login := SocialLogin{
		State: "state-once", Provider: "demo", Verifier: "verifier",
		AuthorizeQuery: "client_id=test", ExpiresAt: time.Now().Add(time.Minute),
	}
	if err := service.db.StartSocialLogin(ctx, login); err != nil {
		t.Fatalf("начало входа: %v", err)
	}

	taken, err := service.db.TakeSocialLogin(ctx, "state-once")
	if err != nil {
		t.Fatalf("чтение состояния: %v", err)
	}
	if taken.Verifier != "verifier" || taken.AuthorizeQuery != "client_id=test" {
		t.Errorf("состояние прочитано неверно: %+v", taken)
	}

	if _, err = service.db.TakeSocialLogin(ctx, "state-once"); !errors.Is(err, ErrSocialState) {
		t.Errorf("состояние сработало повторно: %v", err)
	}
}

func TestSocialStateExpires(t *testing.T) {
	ctx := context.Background()
	service := newSocialService(t)

	if err := service.db.StartSocialLogin(ctx, SocialLogin{
		State: "state-old", Provider: "demo", Verifier: "verifier",
		ExpiresAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("начало входа: %v", err)
	}

	if _, err := service.db.TakeSocialLogin(ctx, "state-old"); !errors.Is(err, ErrSocialState) {
		t.Errorf("просроченное состояние принято: %v", err)
	}

	deleted, err := service.db.DeleteExpiredSocialLogins(ctx)
	if err != nil {
		t.Fatalf("очистка состояний: %v", err)
	}
	if deleted != 1 {
		t.Errorf("удалено %d состояний, ожидалось 1", deleted)
	}
}

// TestUnlinkKeepsAccessible проверяет, что пользователь не может остаться
// без единственного способа входа.
func TestUnlinkKeepsAccessible(t *testing.T) {
	ctx := context.Background()
	service := newSocialService(t)

	userId, err := service.resolveIdentity(ctx, SocialLogin{Provider: "demo"},
		testProfile("ext-unlink", "unlink@example.com"))
	if err != nil {
		t.Fatalf("вход: %v", err)
	}

	// Пароля у этого пользователя нет: отвязать единственный способ
	// входа значит отобрать доступ.
	if err = service.db.UnlinkIdentity(ctx, userId, "demo"); !errors.Is(err, ErrLastIdentity) {
		t.Fatalf("получено %v, ожидалась %v", err, ErrLastIdentity)
	}

	t.Run("вторая идентичность делает отвязку возможной", func(t *testing.T) {
		if err := service.db.LinkIdentity(ctx, userId, SocialProfile{
			Provider: "another", ExternalId: "ext-2", Email: "second@example.com",
		}); err != nil {
			t.Fatalf("привязка второго провайдера: %v", err)
		}
		if err := service.db.UnlinkIdentity(ctx, userId, "demo"); err != nil {
			t.Errorf("отвязка при двух идентичностях: %v", err)
		}
	})

	t.Run("несуществующая привязка не отвязывается", func(t *testing.T) {
		if err := service.db.UnlinkIdentity(ctx, userId, "missing"); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("получено %v, ожидалась %v", err, sql.ErrNoRows)
		}
	})
}

func TestUnlinkAllowedWithPassword(t *testing.T) {
	ctx := context.Background()
	service := newSocialService(t)

	local, err := service.db.CreateUser(ctx, Registration{
		Username:     "local-" + uuid.NewString()[:8],
		PasswordHash: "not-a-real-hash",
		Phone:        "+79004445599",
	})
	if err != nil {
		t.Fatalf("регистрация: %v", err)
	}
	if err = service.db.LinkIdentity(ctx, local.Id, testProfile("ext-pass", "pass@example.com")); err != nil {
		t.Fatalf("привязка: %v", err)
	}

	// У пользователя есть пароль, значит доступ он не потеряет.
	if err = service.db.UnlinkIdentity(ctx, local.Id, "demo"); err != nil {
		t.Errorf("отвязка при заданном пароле: %v", err)
	}
}
