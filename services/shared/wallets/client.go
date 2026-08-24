// Package wallets — клиент сервиса кошелька для остальных сервисов.
//
// Пакет общий, потому что к кошельку ходят и список желаний, и котёл,
// и правила обращения к нему одинаковы: чужой кошелёк читается от имени
// служебного оператора, а средства двигает владелец.
package wallets

import (
	"context"
	"errors"
	"fmt"
	"strings"

	wallet "wish/middleware/wallet/v1"
	"wish/services"
	"wish/services/shared/credit"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Ошибки кошелька, разделённые по тому, что с ними делать вызывающему.
//
// Кошелёк переводит причины отказа в коды gRPC, но без обратного перевода
// здесь эта работа пропадала: у вызывающего оставалась одна ошибка на всё,
// и нехватка средств выглядела как недоступность сервиса. Пользователь
// получал «попробуйте позже» вместо «не хватает денег», а мониторинг —
// отказ там, где всё работало правильно.
var (
	// ErrInsufficientFunds — на кошельке не хватает средств.
	ErrInsufficientFunds = errors.New("insufficient funds")
	// ErrRejected — кошелёк отказал по существу: не тот владелец,
	// кошелёк заблокирован, неверная сумма. Повтор не поможет.
	ErrRejected = errors.New("wallet rejected the operation")
	// ErrUnavailable — кошелёк не ответил или ответил сбоем. Повторить
	// имеет смысл.
	ErrUnavailable = errors.New("wallet is unavailable")
)

// walletError переводит код gRPC в ошибку домена.
//
// Нехватка средств отделена от прочих отказов по существу текстом
// сообщения: FailedPrecondition кошелёк возвращает и на заблокированный
// кошелёк, и на неподходящий резерв, а разница для вызывающего есть —
// в одном случае человеку нужно пополнить кошелёк, в другом ему нечего
// делать вовсе.
func walletError(operation string, err error) error {
	message := status.Convert(err).Message()
	switch status.Code(err) {
	case codes.FailedPrecondition:
		if strings.Contains(message, insufficientBalance) {
			return fmt.Errorf("%w: %s", ErrInsufficientFunds, message)
		}
		return fmt.Errorf("%w: %s", ErrRejected, message)
	case codes.InvalidArgument, codes.NotFound, codes.PermissionDenied:
		return fmt.Errorf("%w: %s", ErrRejected, message)
	default:
		return fmt.Errorf("%w: %s: %w", ErrUnavailable, operation, err)
	}
}

// insufficientBalance — начало сообщения, которым кошелёк отвечает
// на нехватку средств (ErrInsufficientBalance в сервисе кошелька).
// Совпадение по тексту хрупко, но альтернатива — свой код ошибки
// в протоколе, а он один на все отказы по существу.
const insufficientBalance = "insufficient balance"

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
		return Info{}, walletError(fmt.Sprintf("loading wallet of %s", owner), err)
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
		return walletError(fmt.Sprintf("transferring %s from %s", params.Value, owner), err)
	}
	return nil
}

func (c *Client) asOperator(ctx context.Context) context.Context {
	return services.WithAuthorization(ctx, &services.AuthorizedUser{
		Id: c.serviceId, Roles: []string{services.RoleOperator},
	})
}
