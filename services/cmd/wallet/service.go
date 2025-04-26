package main

import (
	"context"
	"log"
	"log/slog"

	"wish/middleware/wallet"
	"wish/services"
)

type service struct {
	wallet.UnimplementedServiceServer
	db    DatabaseClass
	cache services.Cache
}

func (s *service) Classes(ctx context.Context, request *wallet.ClassRequest) (*wallet.ClassReply, error) {
	services.PrintMetadata(ctx)
	var status *string = nil
	if request.Status != nil {
		s2 := request.GetStatus().String()
		status = &s2
	}
	classes, err := s.db.Classes(request.NameFilter, status, request.Version)
	if err != nil {
		slog.Error("Get classes error", slog.String("err", err.Error()))
		return nil, err
	}
	var reply wallet.ClassReply
	for _, element := range classes {
		if reply.Classes == nil {
			reply.Classes = make([]*wallet.Class, 0)
		}
		reply.Classes = append(reply.Classes, &wallet.Class{
			Name:  element.Name,
			Title: element.Title,
		})
	}
	return &reply, nil
}

func (s *service) Elements(ctx context.Context, request *wallet.ClassElementRequest) (*wallet.ClassElementReply, error) {
	services.PrintMetadata(ctx)
	c, err := s.db.Class(request.Name)
	if err != nil {
		slog.Error("Get wallet error ", slog.String("err", err.Error()))
		return nil, err
	}
	var status *string = nil
	if request.Status != nil {
		s2 := request.GetStatus().String()
		status = &s2
	}
	var offset = 0
	if request.Offset != nil {
		offset = int(*request.Offset)
	}
	var limit = 100
	if request.Limit != nil {
		limit = int(*request.Limit)
	}
	elements, next, err := s.db.Elements(*c, request.Version, status, offset, limit)
	if err != nil {
		log.Println(err)
		return nil, err
	}
	var reply wallet.ClassElementReply
	for _, element := range elements {
		if reply.Elements == nil {
			reply.Elements = make([]*wallet.ClassElement, 0)
		}
		reply.Elements = append(reply.Elements, &wallet.ClassElement{
			Key:     element.Key,
			Value:   element.Value,
			Version: element.Version,
		})
	}
	reply.NextOffset = uint32(next)
	reply.Eof = len(elements) < limit
	return &reply, nil
}
