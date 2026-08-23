// Package wallets — клиент сервиса кошелька для остальных сервисов.
//
// Пакет общий, потому что к кошельку ходят и список желаний, и котёл,
// и правила обращения к нему одинаковы: чужой кошелёк читается от имени
// служебного оператора, а средства двигает владелец.
package wallets

import (
	"context"
	"fmt"

	wallet "wish/middleware/wallet/v1"
	"wish/services"
	"wish/services/shared/credit"

	"github.com/google/uuid"
	"google.golang.org/grpc"
)

// Info — то, что нужно знать о кошельке.
type Info struct {
	Id      uuid.UUID     `json:"id"`
	Balance credit.Amount `json:"balance"`
	// Available — остаток за вычетом действующих резервов.
	Available credit.Amount `json:"available"`
}

// TransferParams — перевод между кошельками.
type TransferParams struct {
	// IdempotencyKey обязателен: повтор запроса при обрыве связи не должен
	// провести перевод дважды.
	IdempotencyKey string
	Source         uuid.UUID
	Target         uuid.UUID
	Value          credit.Amount
	Message        string
}

// Client обращается к сервису кошелька.
type Client struct {
	client wallet.ServiceClient
	// serviceId — от чьего имени читается чужой кошелёк: он виден только
	// оператору, и служебный идентификатор нужен ровно для этого.
	serviceId uuid.UUID
}

func NewClient(conn *grpc.ClientConn, serviceId uuid.UUID) *Client {
	return &Client{client: wallet.NewServiceClient(conn), serviceId: serviceId}
}

// Wallet возвращает кошелёк владельца, создавая его при первом обращении:
// сервис кошелька заводит кошелёк сам, когда его спрашивают.
//
// Владельцем выступает не только человек: у котла тоже есть свой кошелёк,
// и он ничем не отличается от пользовательского.
func (c *Client) Wallet(ctx context.Context, owner uuid.UUID) (Info, error) {
	id := owner.String()
	replies, err := c.client.Information(c.asOperator(ctx), &wallet.InformationRequest{UserId: &id})
	if err != nil {
		return Info{}, fmt.Errorf("loading wallet of %s: %w", owner, err)
	}

	for _, reply := range replies.GetReplies() {
		if reply.GetType() != wallet.WalletType_USER || reply.GetState() != wallet.WalletState_ACTIVE {
			continue
		}
		parsed, err := uuid.Parse(reply.GetId())
		if err != nil {
			return Info{}, fmt.Errorf("parsing wallet id %q: %w", reply.GetId(), err)
		}
		return Info{
			Id:        parsed,
			Balance:   credit.Amount(reply.GetBalance()),
			Available: credit.Amount(reply.GetAvailable()),
		}, nil
	}
	return Info{}, fmt.Errorf("%s has no active wallet", owner)
}

// Transfer переводит средства от имени владельца исходного кошелька:
// кошелёк проверяет, что списывают у того, кто об этом просит.
func (c *Client) Transfer(ctx context.Context, owner uuid.UUID, params TransferParams) error {
	_, err := c.client.Transfer(
		services.WithAuthorization(ctx, &services.AuthorizedUser{Id: owner}),
		&wallet.TransferRequest{
			IdempotencyKey: params.IdempotencyKey,
			SourceWalletId: params.Source.String(),
			TargetWalletId: params.Target.String(),
			Value:          int64(params.Value),
			Message:        params.Message,
		})
	if err != nil {
		return fmt.Errorf("transferring %s from %s: %w", params.Value, owner, err)
	}
	return nil
}

func (c *Client) asOperator(ctx context.Context) context.Context {
	return services.WithAuthorization(ctx, &services.AuthorizedUser{
		Id: c.serviceId, Roles: []string{services.RoleOperator},
	})
}
