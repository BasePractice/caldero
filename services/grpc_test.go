package services

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// incoming собирает контекст входящего вызова так, как его видит сервер.
func incoming(id string, roles ...string) context.Context {
	pairs := []string{"x-authorized-id", id}
	for _, role := range roles {
		pairs = append(pairs, "x-roles", role)
	}
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(pairs...))
}

// TestAuthorizeUnaryInterceptor фиксирует смысл перехватчика: проверка
// делается один раз на входе, а не в каждом методе — забыть её в новом
// методе иначе можно одной строкой.
func TestAuthorizeUnaryInterceptor(t *testing.T) {
	user := uuid.New()
	info := &grpc.UnaryServerInfo{FullMethod: "/wallet.v1.Service/Transfer"}
	interceptor := AuthorizeUnaryInterceptor("/wallet.v1.Service/Health")

	t.Run("проверенный пользователь доходит до обработчика", func(t *testing.T) {
		_, err := interceptor(incoming(user.String(), RoleOperator), nil, info,
			func(ctx context.Context, _ any) (any, error) {
				authorized, ok := AuthorizedFromContext(ctx)
				if !ok {
					t.Error("пользователь не положен в контекст")
					return nil, nil
				}
				if authorized.Id != user {
					t.Errorf("пользователь %s, ожидался %s", authorized.Id, user)
				}
				if !authorized.HasRole(RoleOperator) {
					t.Errorf("роли %v, ожидалась %s", authorized.Roles, RoleOperator)
				}
				return nil, nil
			})
		if err != nil {
			t.Fatalf("вызов отклонён: %v", err)
		}
	})

	t.Run("вызов без метаданных отклоняется", func(t *testing.T) {
		called := false
		_, err := interceptor(context.Background(), nil, info,
			func(context.Context, any) (any, error) {
				called = true
				return nil, nil
			})
		if status.Code(err) != codes.Unauthenticated {
			t.Errorf("код %s, ожидался %s", status.Code(err), codes.Unauthenticated)
		}
		if called {
			t.Error("обработчик вызван для непроверенного вызова")
		}
	})

	t.Run("неразбираемый идентификатор отклоняется", func(t *testing.T) {
		_, err := interceptor(incoming("не-uuid"), nil, info,
			func(context.Context, any) (any, error) { return nil, nil })
		if status.Code(err) != codes.Unauthenticated {
			t.Errorf("код %s, ожидался %s", status.Code(err), codes.Unauthenticated)
		}
	})

	t.Run("нулевой идентификатор отклоняется", func(t *testing.T) {
		_, err := interceptor(incoming(uuid.Nil.String()), nil, info,
			func(context.Context, any) (any, error) { return nil, nil })
		if status.Code(err) != codes.Unauthenticated {
			t.Errorf("код %s, ожидался %s", status.Code(err), codes.Unauthenticated)
		}
	})

	t.Run("исключённый метод проверку не проходит", func(t *testing.T) {
		called := false
		_, err := interceptor(context.Background(), nil,
			&grpc.UnaryServerInfo{FullMethod: "/wallet.v1.Service/Health"},
			func(ctx context.Context, _ any) (any, error) {
				called = true
				// Исключённый метод пользователя не получает: он и не должен
				// его требовать.
				if _, ok := AuthorizedFromContext(ctx); ok {
					t.Error("исключённый метод получил пользователя")
				}
				return nil, nil
			})
		if err != nil {
			t.Fatalf("исключённый метод отклонён: %v", err)
		}
		if !called {
			t.Error("исключённый метод не дошёл до обработчика")
		}
	})

	t.Run("ошибка обработчика доходит как есть", func(t *testing.T) {
		want := errors.New("сбой обработчика")
		_, err := interceptor(incoming(user.String()), nil, info,
			func(context.Context, any) (any, error) { return nil, want })
		if !errors.Is(err, want) {
			t.Errorf("ошибка %v, ожидалась %v", err, want)
		}
	})
}

func TestAuthorizedFromContextEmpty(t *testing.T) {
	if _, ok := AuthorizedFromContext(context.Background()); ok {
		t.Error("пустой контекст вернул пользователя")
	}
}

// TestPrintMetadata: вывод метаданных включается только уровнем DEBUG,
// потому что они содержат заголовки вызова.
func TestPrintMetadata(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })

	logger, err := DefineLogging(Config{LogLevel: "DEBUG"})
	if err != nil {
		t.Fatalf("настройка журнала: %v", err)
	}
	slog.SetDefault(logger)

	PrintMetadata(incoming(uuid.New().String(), RoleOperator))
	// Контекст без метаданных: ветка «нечего печатать» тоже должна
	// отрабатывать без паники.
	PrintMetadata(context.Background())

	quiet, err := DefineLogging(Config{LogLevel: "ERROR"})
	if err != nil {
		t.Fatalf("настройка журнала: %v", err)
	}
	slog.SetDefault(quiet)
	PrintMetadata(incoming(uuid.New().String()))
}
