package services

import (
	"context"
	"log/slog"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// authorizedKey — приватный тип ключа контекста для пользователя.
type authorizedKey struct{}

// AuthorizeUnaryInterceptor проверяет вызывающего один раз на входе.
// Раньше каждый метод делал это сам, и забыть проверку в новом методе
// было делом одной строки.
func AuthorizeUnaryInterceptor(exempt ...string) grpc.UnaryServerInterceptor {
	skip := make(map[string]struct{}, len(exempt))
	for _, method := range exempt {
		skip[method] = struct{}{}
	}

	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		if _, ok := skip[info.FullMethod]; ok {
			return handler(ctx, req)
		}
		user, err := GrpcAuthorized(ctx)
		if err != nil {
			slog.DebugContext(ctx, "Unauthorized grpc call",
				slog.String("method", info.FullMethod), slog.String("err", err.Error()))
			return nil, status.Error(codes.Unauthenticated, "not authorized")
		}
		return handler(context.WithValue(ctx, authorizedKey{}, user), req)
	}
}

// AuthorizedFromContext возвращает пользователя, проверенного интерсептором.
func AuthorizedFromContext(ctx context.Context) (*AuthorizedUser, bool) {
	user, ok := ctx.Value(authorizedKey{}).(*AuthorizedUser)
	return user, ok
}

func PrintMetadata(context context.Context) {
	if slog.Default().Enabled(context, slog.LevelDebug) {
		md, ok := metadata.FromIncomingContext(context)
		if ok {
			var args = make([]any, 0)
			for key, values := range md {
				args = append(args, slog.String(key, strings.Join(values, ";")))
			}
			slog.Debug("Metadata", args...)
		}
	}
}
