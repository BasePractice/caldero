package services

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"google.golang.org/grpc/metadata"
	"net/http"
)

type AuthorizedUser struct {
	Id uuid.UUID
}

func HttpAuthorized(request *http.Request) (*AuthorizedUser, error) {
	var userId = request.Header.Get("X-Authorized-Id")
	id, err := uuid.Parse(userId)
	if err != nil {
		return nil, errors.New(fmt.Sprintf("invalid user id: %s", err.Error()))
	} else if id == uuid.Nil {
		return nil, errors.New("invalid user id: was nil")
	}
	return &AuthorizedUser{id}, nil
}

func GrpcAuthorized(context context.Context) (*AuthorizedUser, error) {
	md, ok := metadata.FromIncomingContext(context)
	if ok {
		auths := md.Get("x-authorized-id")
		if len(auths) > 0 {
			//after, found := strings.CutPrefix(auths[0], "Bearer ")
			//if found {
			//	return &after
			//}
			id, err := uuid.Parse(auths[0])
			if err != nil {
				return nil, errors.New(fmt.Sprintf("invalid user id: %s", err.Error()))
			}
			return &AuthorizedUser{id}, nil
		}
	}
	return nil, errors.New("no authorized id")
}
