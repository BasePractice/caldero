package main

import (
	"context"
	"log/slog"

	"wish/middleware/wallet"
	"wish/services"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type service struct {
	wallet.UnimplementedServiceServer
	db    DatabaseWallet
	cache services.Cache
}

func (s service) Information(ctx context.Context, request *wallet.InformationRequest) (*wallet.InformationReplyList, error) {
	authorized, err := services.GrpcAuthorized(ctx)
	if err != nil {
		slog.Debug("Unauthorized information request", slog.String("err", err.Error()))
		return nil, status.Error(codes.Unauthenticated, "not authorized")
	}

	if request.UserId != nil {
		requested, err := uuid.Parse(*request.UserId)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "user_id is not a valid uuid")
		}
		// Кошелёк доступен только владельцу: без этой проверки достаточно
		// подставить чужой uuid, чтобы прочитать чужой баланс и транзакции.
		if requested != authorized.Id {
			slog.Warn("Attempt to read foreign wallet",
				slog.String("authorized", authorized.Id.String()))
			return nil, status.Error(codes.PermissionDenied, "wallet belongs to another user")
		}
	}

	replies := make([]*wallet.InformationReply, 0)
	if err = s.db.Information(authorized.Id, func(reply *wallet.InformationReply) {
		replies = append(replies, reply)
	}); err != nil {
		slog.Error("Failed to load wallets", slog.String("err", err.Error()))
		return nil, status.Error(codes.Internal, "can't load wallets")
	}
	return &wallet.InformationReplyList{Replies: replies}, nil
}
