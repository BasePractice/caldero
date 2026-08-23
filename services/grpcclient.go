package services

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// grpcCallTimeout — предел на один вызов, если вызывающий не задал свой.
// Вызов без дедлайна висит до разрыва соединения и держит горутину.
const grpcCallTimeout = 3 * time.Second

// NewGrpcClient открывает соединение с сервисом.
//
// TLS не используется: см. принятый риск в docs/threat-model.md. Соединение
// живёт в пределах внутренней сети, и это ограничение снимается вместе
// с выходом за пределы одной машины.
func NewGrpcClient(address string) (*grpc.ClientConn, error) {
	conn, err := grpc.NewClient(address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithChainUnaryInterceptor(
			propagateAuthorization,
			deadlineInterceptor,
		),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(MaxRequestBody)),
	)
	if err != nil {
		return nil, fmt.Errorf("creating grpc client for %s: %w", address, err)
	}
	return conn, nil
}

// propagateAuthorization переносит идентификатор и роли вызывающего
// в исходящий вызов. Без этого сервис на той стороне видит вызов
// без пользователя и не может проверить права.
func propagateAuthorization(
	ctx context.Context,
	method string,
	req, reply any,
	conn *grpc.ClientConn,
	invoker grpc.UnaryInvoker,
	opts ...grpc.CallOption,
) error {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		outgoing := metadata.MD{}
		for _, key := range []string{"x-authorized-id", "x-roles"} {
			if values := md.Get(key); len(values) > 0 {
				outgoing.Set(key, values...)
			}
		}
		if len(outgoing) > 0 {
			ctx = metadata.NewOutgoingContext(ctx, outgoing)
		}
	}
	return invoker(ctx, method, req, reply, conn, opts...)
}

// WithAuthorization добавляет идентификатор вызывающего в исходящий контекст.
// Нужен там, где вызов начинается не из gRPC-запроса, а из HTTP-обработчика.
func WithAuthorization(ctx context.Context, user *AuthorizedUser) context.Context {
	md := metadata.Pairs("x-authorized-id", user.Id.String())
	for _, role := range user.Roles {
		md.Append("x-roles", role)
	}
	return metadata.NewOutgoingContext(ctx, md)
}

func deadlineInterceptor(
	ctx context.Context,
	method string,
	req, reply any,
	conn *grpc.ClientConn,
	invoker grpc.UnaryInvoker,
	opts ...grpc.CallOption,
) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, grpcCallTimeout)
		defer cancel()
	}
	return invoker(ctx, method, req, reply, conn, opts...)
}

// CloseGrpcClient закрывает соединение и логирует ошибку.
func CloseGrpcClient(name string, conn *grpc.ClientConn) {
	if err := conn.Close(); err != nil {
		slog.Error("Can't close grpc client",
			slog.String("service", name), slog.String("err", err.Error()))
	}
}
