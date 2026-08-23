package main

import (
	"context"
	"fmt"

	wallet "wish/middleware/wallet/v1"
	"wish/services"
	"wish/services/shared/credit"

	"github.com/google/uuid"
	"google.golang.org/grpc"
)

// walletClient обращается к сервису кошелька.
type walletClient struct {
	client wallet.ServiceClient
	// serviceId — от чьего имени читается чужой кошелёк. Своим кошельком
	// распоряжается сам даритель, а вот кошелёк одаряемого виден только
	// оператору, и служебный идентификатор нужен ровно для этого.
	serviceId uuid.UUID
}

func NewWalletClient(conn *grpc.ClientConn, serviceId uuid.UUID) Wallet {
	return &walletClient{client: wallet.NewServiceClient(conn), serviceId: serviceId}
}

func (w *walletClient) Wallet(ctx context.Context, user uuid.UUID) (WalletInfo, error) {
	id := user.String()
	replies, err := w.client.Information(
		services.WithAuthorization(ctx, &services.AuthorizedUser{
			Id: w.serviceId, Roles: []string{services.RoleOperator},
		}),
		&wallet.InformationRequest{UserId: &id})
	if err != nil {
		return WalletInfo{}, fmt.Errorf("loading wallet of user %s: %w", user, err)
	}

	for _, reply := range replies.GetReplies() {
		if reply.GetType() != wallet.WalletType_USER || reply.GetState() != wallet.WalletState_ACTIVE {
			continue
		}
		parsed, err := uuid.Parse(reply.GetId())
		if err != nil {
			return WalletInfo{}, fmt.Errorf("parsing wallet id %q: %w", reply.GetId(), err)
		}
		return WalletInfo{Id: parsed, Available: credit.Amount(reply.GetAvailable())}, nil
	}
	return WalletInfo{}, fmt.Errorf("user %s has no active wallet", user)
}

func (w *walletClient) Transfer(ctx context.Context, giver uuid.UUID, params TransferParams) error {
	// Перевод идёт от имени дарителя: подарок делает он, а не система
	// за него, и кошелёк проверяет, что списывают у владельца.
	_, err := w.client.Transfer(
		services.WithAuthorization(ctx, &services.AuthorizedUser{Id: giver}),
		&wallet.TransferRequest{
			IdempotencyKey: params.IdempotencyKey,
			SourceWalletId: params.Source.String(),
			TargetWalletId: params.Target.String(),
			Value:          int64(params.Value),
			Message:        params.Message,
		})
	if err != nil {
		return fmt.Errorf("transferring %s from %s: %w", params.Value, giver, err)
	}
	return nil
}
