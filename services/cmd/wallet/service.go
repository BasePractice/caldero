package main

import (
	"context"
	"log/slog"

	wallet "wish/middleware/wallet/v1"
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
	authorized, ok := services.AuthorizedFromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "not authorized")
	}

	var err error
	owner := authorized.Id
	if request.UserId != nil {
		if owner, err = uuid.Parse(*request.UserId); err != nil {
			return nil, status.Error(codes.InvalidArgument, "user_id is not a valid uuid")
		}
		// Чужой кошелёк доступен только оператору: без проверки достаточно
		// подставить чужой uuid, чтобы прочитать чужой баланс и транзакции.
		if !authorized.CanActOnBehalfOf(owner) {
			slog.WarnContext(ctx, "Attempt to read foreign wallet",
				slog.String("authorized", authorized.Id.String()))
			return nil, status.Error(codes.PermissionDenied, "wallet belongs to another user")
		}
	}

	replies := make([]*wallet.InformationReply, 0)
	if err = s.db.Information(ctx, owner, func(reply *wallet.InformationReply) {
		replies = append(replies, reply)
	}); err != nil {
		slog.ErrorContext(ctx, "Failed to load wallets", slog.String("err", err.Error()))
		return nil, status.Error(codes.Internal, "can't load wallets")
	}
	return &wallet.InformationReplyList{Replies: replies}, nil
}
