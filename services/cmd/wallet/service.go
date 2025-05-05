package main

import (
	"context"

	"wish/middleware/wallet"
	"wish/services"

	"github.com/google/uuid"
)

type service struct {
	wallet.UnimplementedServiceServer
	db    DatabaseWallet
	cache services.Cache
}

func (s service) Information(ctx context.Context, request *wallet.InformationRequest) (*wallet.InformationReplyList, error) {
	var err error
	authorized, err := services.GrpcAuthorized(ctx)
	if err != nil {
		return nil, err
	}
	var userId uuid.UUID
	if request.UserId == nil {
		userId = authorized.Id
	} else {
		userId, err = uuid.Parse(*request.UserId)
		if err != nil {
			return nil, err
		}
	}
	var replies = make([]*wallet.InformationReply, 0)

	err = s.db.Information(userId, func(reply *wallet.InformationReply) {
		replies = append(replies, reply)
	})
	if err != nil {
		return nil, err
	}
	return &wallet.InformationReplyList{Replies: replies}, nil
}
