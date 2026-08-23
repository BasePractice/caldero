package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

func TestNewGrpcClient(t *testing.T) {
	// grpc.NewClient соединение не устанавливает, поэтому живой сервис
	// для проверки сборки клиента не нужен.
	conn, err := NewGrpcClient("passthrough:///wallet:51051")
	if err != nil {
		t.Fatalf("создание клиента: %v", err)
	}
	CloseGrpcClient("wallet", conn)
	// Повторное закрытие обязано попасть в журнал, а не уронить процесс.
	CloseGrpcClient("wallet", conn)
}

// TestNewGrpcClientBadAddress: адрес приходит из конфигурации, и разбор
// цели обязан быть ошибкой старта, а не отложенной ошибкой первого вызова.
func TestNewGrpcClientBadAddress(t *testing.T) {
	if _, err := NewGrpcClient("\x7f"); err == nil {
		t.Fatal("некорректный адрес принят")
	}
}

// TestPropagateAuthorization фиксирует перенос вызывающего в исходящий вызов:
// без него сервис на той стороне видит вызов без пользователя и не может
// проверить права.
func TestPropagateAuthorization(t *testing.T) {
	user := uuid.New()

	var got metadata.MD
	invoker := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		got, _ = metadata.FromOutgoingContext(ctx)
		return nil
	}

	err := propagateAuthorization(incoming(user.String(), RoleOperator, RoleAdmin),
		"/wallet.v1.Service/Transfer", nil, nil, nil, invoker)
	if err != nil {
		t.Fatalf("вызов: %v", err)
	}

	if values := got.Get("x-authorized-id"); len(values) != 1 || values[0] != user.String() {
		t.Errorf("идентификатор %v, ожидался %s", values, user)
	}
	if values := got.Get("x-roles"); len(values) != 2 {
		t.Errorf("роли %v, ожидались обе", values)
	}
}

// TestPropagateAuthorizationWithoutMetadata: вызов, начатый не из gRPC-запроса,
// не должен получать пустых заголовков.
func TestPropagateAuthorizationWithoutMetadata(t *testing.T) {
	var had bool
	invoker := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		_, had = metadata.FromOutgoingContext(ctx)
		return nil
	}

	if err := propagateAuthorization(context.Background(), "/m", nil, nil, nil, invoker); err != nil {
		t.Fatalf("вызов: %v", err)
	}
	if had {
		t.Error("в исходящий контекст добавлены пустые метаданные")
	}

	// Входящие метаданные без нужных ключей тоже не должны порождать
	// исходящих.
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("user-agent", "test"))
	if err := propagateAuthorization(ctx, "/m", nil, nil, nil, invoker); err != nil {
		t.Fatalf("вызов: %v", err)
	}
	if had {
		t.Error("посторонние метаданные перенесены в исходящий вызов")
	}
}

func TestPropagateAuthorizationError(t *testing.T) {
	want := errors.New("сбой вызова")
	invoker := func(context.Context, string, any, any, *grpc.ClientConn, ...grpc.CallOption) error {
		return want
	}

	err := propagateAuthorization(context.Background(), "/m", nil, nil, nil, invoker)
	if !errors.Is(err, want) {
		t.Errorf("ошибка %v, ожидалась %v", err, want)
	}
}

// TestDeadlineInterceptor: вызов без дедлайна висит до разрыва соединения
// и держит горутину, поэтому предел ставится по умолчанию.
func TestDeadlineInterceptor(t *testing.T) {
	t.Run("своего дедлайна нет — ставится предел по умолчанию", func(t *testing.T) {
		var deadline time.Time
		var ok bool
		invoker := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
			deadline, ok = ctx.Deadline()
			return nil
		}

		if err := deadlineInterceptor(context.Background(), "/m", nil, nil, nil, invoker); err != nil {
			t.Fatalf("вызов: %v", err)
		}
		if !ok {
			t.Fatal("дедлайн не поставлен")
		}
		if left := time.Until(deadline); left <= 0 || left > grpcCallTimeout {
			t.Errorf("до дедлайна %s, ожидалось не больше %s", left, grpcCallTimeout)
		}
	})

	t.Run("свой дедлайн не перебивается", func(t *testing.T) {
		want := time.Now().Add(time.Hour)
		ctx, cancel := context.WithDeadline(context.Background(), want)
		defer cancel()

		var got time.Time
		invoker := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
			got, _ = ctx.Deadline()
			return nil
		}

		if err := deadlineInterceptor(ctx, "/m", nil, nil, nil, invoker); err != nil {
			t.Fatalf("вызов: %v", err)
		}
		if !got.Equal(want) {
			t.Errorf("дедлайн %s, ожидался %s", got, want)
		}
	})
}

func TestWithAuthorization(t *testing.T) {
	user := &AuthorizedUser{Id: uuid.New(), Roles: []string{RoleOperator, RoleAdmin}}

	md, ok := metadata.FromOutgoingContext(WithAuthorization(context.Background(), user))
	if !ok {
		t.Fatal("метаданные не добавлены")
	}
	if values := md.Get("x-authorized-id"); len(values) != 1 || values[0] != user.Id.String() {
		t.Errorf("идентификатор %v, ожидался %s", values, user.Id)
	}
	if values := md.Get("x-roles"); len(values) != 2 {
		t.Errorf("роли %v, ожидались обе", values)
	}

	// Пользователь без ролей — обычный случай: заголовок ролей должен
	// просто отсутствовать.
	md, _ = metadata.FromOutgoingContext(
		WithAuthorization(context.Background(), &AuthorizedUser{Id: uuid.New()}))
	if values := md.Get("x-roles"); len(values) != 0 {
		t.Errorf("роли %v, ожидалось пусто", values)
	}
}
