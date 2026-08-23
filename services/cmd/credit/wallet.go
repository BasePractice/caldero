package main

import (
	"context"
	"errors"
	"fmt"

	wallet "wish/middleware/wallet/v1"
	"wish/services"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Wallet — то, что сервису кредитов нужно от кошелька. Интерфейс объявлен
// здесь, у потребителя, а не рядом с реализацией: кошельку незачем знать,
// кто и как им пользуется.
type Wallet interface {
	Credit(ctx context.Context, request *wallet.OperationRequest, opts ...grpc.CallOption) (*wallet.TransactionReply, error)
}

// ErrInsufficientFunds — на кошельке не хватает средств.
var ErrInsufficientFunds = errors.New("insufficient funds")

// walletCredit списывает сумму с кошелька пользователя.
//
// Ключ идемпотентности строится из идентификаторов кредита и платежа,
// а не случайно: повтор запроса от клиента должен дать тот же ключ,
// иначе средства спишутся дважды.
func walletCredit(
	ctx context.Context,
	client Wallet,
	operator *services.AuthorizedUser,
	idempotencyKey string,
	value int64,
	message string,
) error {
	if client == nil {
		return fmt.Errorf("wallet service is not configured")
	}

	_, err := client.Credit(services.WithAuthorization(ctx, operator), &wallet.OperationRequest{
		IdempotencyKey: idempotencyKey,
		Value:          value,
		Message:        message,
	})
	if err == nil {
		return nil
	}

	// Коды gRPC переводятся в ошибки домена: обработчику незачем знать
	// про транспорт, а клиенту нужна причина, а не «Internal».
	switch status.Code(err) {
	case codes.FailedPrecondition:
		return fmt.Errorf("%w: %s", ErrInsufficientFunds, status.Convert(err).Message())
	case codes.NotFound:
		return fmt.Errorf("wallet not found: %w", err)
	default:
		return fmt.Errorf("debiting wallet: %w", err)
	}
}
