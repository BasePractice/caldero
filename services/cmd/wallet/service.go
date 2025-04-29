package main

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"wish/middleware/wallet"
	"wish/services"
)

type service struct {
	wallet.UnimplementedServiceServer
	db    DatabaseWallet
	cache services.Cache
}

func (s service) Information(ctx context.Context, request *wallet.InformationRequest) (*wallet.InformationReplyList, error) {
	services.PrintMetadata(ctx)
	var err error
	var userId uuid.UUID
	if request.UserId == nil {
		id := services.GetUserId(ctx)
		if id != nil {
			userId, err = uuid.Parse(*id)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, errors.New("user id not found")
		}
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
