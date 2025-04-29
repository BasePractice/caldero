package services

import (
	"context"
	"log/slog"
	"strings"

	"google.golang.org/grpc/metadata"
)

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

func GetUserId(context context.Context) *string {
	md, ok := metadata.FromIncomingContext(context)
	if ok {
		auths := md.Get("x-user-id")
		if len(auths) > 0 {
			//after, found := strings.CutPrefix(auths[0], "Bearer ")
			//if found {
			//	return &after
			//}
			return &auths[0]
		}
	}
	return nil
}
