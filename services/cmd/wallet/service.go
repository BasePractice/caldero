package main

import (
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"log/slog"
	"wish/middleware/wallet"
	"wish/services"
)

type service struct {
	wallet.UnimplementedServiceServer
	db    DatabaseWallet
	cache services.Cache
}

func (s service) Information(request *wallet.InformationRequest, response grpc.ServerStreamingServer[wallet.InformationReply]) error {
	userId, err := uuid.Parse(request.UserId)
	if err != nil {
		return err
	}
	return s.db.Information(userId, func(reply *wallet.InformationReply) {
		err = response.Send(reply)
		if err != nil {
			slog.Error("Failed to send reply",
				slog.String("user_id", userId.String()), slog.String("reason", err.Error()))
		}
	})
}
