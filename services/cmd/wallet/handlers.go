package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	wallet "wish/middleware/wallet/v1"
	"wish/services"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// operationError переводит ошибку домена в код gRPC. Без этого клиент
// на любую причину получает Unknown и не может отличить нехватку средств
// от сбоя базы.
func operationError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, ErrInvalidValue):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, ErrSameWallet):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, ErrWalletNotFound):
		return status.Error(codes.NotFound, "wallet not found")
	case errors.Is(err, ErrReservationNotFound):
		return status.Error(codes.NotFound, "reservation not found")
	case errors.Is(err, ErrReservationNotPending):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, ErrWalletNotActive):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, ErrInsufficientBalance):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		slog.ErrorContext(ctx, "Wallet operation failed", slog.String("err", err.Error()))
		return status.Error(codes.Internal, "operation failed")
	}
}

func parseWalletId(value *string) (uuid.UUID, error) {
	if value == nil || *value == "" {
		return uuid.Nil, nil
	}
	id, err := uuid.Parse(*value)
	if err != nil {
		return uuid.Nil, status.Error(codes.InvalidArgument, "wallet_id is not a valid uuid")
	}
	return id, nil
}

func (s service) operationParams(request *wallet.OperationRequest) (OperationParams, error) {
	if request.IdempotencyKey == "" {
		// Без ключа повтор при обрыве связи проведёт операцию второй раз,
		// а клиент не может отличить «запрос не дошёл» от «ответ не дошёл».
		return OperationParams{}, status.Error(codes.InvalidArgument, "idempotency_key is required")
	}
	walletId, err := parseWalletId(request.WalletId)
	if err != nil {
		return OperationParams{}, err
	}
	return OperationParams{
		IdempotencyKey: request.IdempotencyKey,
		WalletId:       walletId,
		Value:          request.Value,
		Message:        request.Message,
	}, nil
}

func (s service) Debit(ctx context.Context, request *wallet.OperationRequest) (*wallet.TransactionReply, error) {
	authorized, ok := services.AuthorizedFromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "not authorized")
	}
	params, err := s.operationParams(request)
	if err != nil {
		return nil, err
	}

	transaction, err := s.db.Debit(ctx, authorized.Id, params)
	if err != nil {
		return nil, operationError(ctx, err)
	}
	return transactionReply(transaction), nil
}

func (s service) Credit(ctx context.Context, request *wallet.OperationRequest) (*wallet.TransactionReply, error) {
	authorized, ok := services.AuthorizedFromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "not authorized")
	}
	params, err := s.operationParams(request)
	if err != nil {
		return nil, err
	}

	transaction, err := s.db.Credit(ctx, authorized.Id, params)
	if err != nil {
		return nil, operationError(ctx, err)
	}
	return transactionReply(transaction), nil
}

func (s service) Transfer(ctx context.Context, request *wallet.TransferRequest) (*wallet.TransactionReply, error) {
	authorized, ok := services.AuthorizedFromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "not authorized")
	}
	if request.IdempotencyKey == "" {
		return nil, status.Error(codes.InvalidArgument, "idempotency_key is required")
	}
	source, err := uuid.Parse(request.SourceWalletId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "source_wallet_id is not a valid uuid")
	}
	target, err := uuid.Parse(request.TargetWalletId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "target_wallet_id is not a valid uuid")
	}

	transaction, err := s.db.Transfer(ctx, authorized.Id, TransferParams{
		IdempotencyKey: request.IdempotencyKey,
		SourceId:       source,
		TargetId:       target,
		Value:          request.Value,
		Message:        request.Message,
	})
	if err != nil {
		return nil, operationError(ctx, err)
	}
	return transactionReply(transaction), nil
}

func (s service) History(ctx context.Context, request *wallet.HistoryRequest) (*wallet.HistoryReply, error) {
	authorized, ok := services.AuthorizedFromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "not authorized")
	}
	walletId, err := parseWalletId(request.WalletId)
	if err != nil {
		return nil, err
	}

	before := request.Before
	var cursor *time.Time
	if before != nil {
		value := before.AsTime()
		cursor = &value
	}

	transactions, err := s.db.History(ctx, authorized.Id, walletId, int(request.Limit), cursor)
	if err != nil {
		return nil, operationError(ctx, err)
	}

	reply := &wallet.HistoryReply{
		Transactions: make([]*wallet.TransactionReply, 0, len(transactions)),
	}
	for _, transaction := range transactions {
		reply.Transactions = append(reply.Transactions, transactionReply(transaction))
	}
	// Курсор отдаётся, только если страница заполнена целиком: иначе
	// клиент сделает ещё один заведомо пустой запрос.
	if len(transactions) > 0 && len(transactions) == int(request.Limit) {
		reply.NextBefore = timestamppb.New(transactions[len(transactions)-1].CreatedAt)
	}
	return reply, nil
}

func (s service) ChangeState(ctx context.Context, request *wallet.ChangeStateRequest) (*wallet.InformationReply, error) {
	authorized, ok := services.AuthorizedFromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "not authorized")
	}
	walletId, err := uuid.Parse(request.WalletId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "wallet_id is not a valid uuid")
	}
	state := request.State.String()
	if request.State == wallet.WalletState_UNKNOWN_STATE {
		return nil, status.Error(codes.InvalidArgument, "state is required")
	}

	if err = s.db.ChangeState(ctx, authorized.Id, walletId, state); err != nil {
		return nil, operationError(ctx, err)
	}

	var reply *wallet.InformationReply
	if err = s.db.Information(ctx, authorized.Id, func(r *wallet.InformationReply) {
		if r.Id == walletId.String() {
			reply = r
		}
	}); err != nil {
		return nil, operationError(ctx, err)
	}
	if reply == nil {
		return nil, status.Error(codes.NotFound, "wallet not found")
	}
	return reply, nil
}

func transactionReply(t Transaction) *wallet.TransactionReply {
	reply := &wallet.TransactionReply{
		Id:        t.Id.String(),
		WalletId:  t.WalletId.String(),
		Operation: wallet.Operation(wallet.Operation_value[string(t.Operation)]),
		State:     wallet.TransactionState(wallet.TransactionState_value[t.State]),
		Value:     t.Value,
		Balance:   t.Balance,
		Message:   t.Message,
		CreatedAt: timestamppb.New(t.CreatedAt),
	}
	if t.SourceId != nil {
		source := t.SourceId.String()
		reply.SourceWalletId = &source
	}
	return reply
}

func (s service) Reserve(ctx context.Context, request *wallet.ReserveRequest) (*wallet.TransactionReply, error) {
	authorized, ok := services.AuthorizedFromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "not authorized")
	}
	if request.IdempotencyKey == "" {
		return nil, status.Error(codes.InvalidArgument, "idempotency_key is required")
	}
	walletId, err := parseWalletId(request.WalletId)
	if err != nil {
		return nil, err
	}

	transaction, err := s.db.Reserve(ctx, authorized.Id, ReserveParams{
		IdempotencyKey: request.IdempotencyKey,
		WalletId:       walletId,
		Value:          request.Value,
		Message:        request.Message,
		TTL:            time.Duration(request.TtlSeconds) * time.Second,
	})
	if err != nil {
		return nil, operationError(ctx, err)
	}
	return transactionReply(transaction), nil
}

func (s service) Confirm(ctx context.Context, request *wallet.SettleRequest) (*wallet.TransactionReply, error) {
	return s.settle(ctx, request, func(ctx context.Context, owner, id uuid.UUID) (Transaction, error) {
		return s.db.Confirm(ctx, owner, id)
	})
}

func (s service) Reject(ctx context.Context, request *wallet.SettleRequest) (*wallet.TransactionReply, error) {
	return s.settle(ctx, request, func(ctx context.Context, owner, id uuid.UUID) (Transaction, error) {
		return s.db.Reject(ctx, owner, id)
	})
}

func (s service) settle(
	ctx context.Context,
	request *wallet.SettleRequest,
	action func(context.Context, uuid.UUID, uuid.UUID) (Transaction, error),
) (*wallet.TransactionReply, error) {
	authorized, ok := services.AuthorizedFromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "not authorized")
	}
	transactionId, err := uuid.Parse(request.TransactionId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "transaction_id is not a valid uuid")
	}

	transaction, err := action(ctx, authorized.Id, transactionId)
	if err != nil {
		return nil, operationError(ctx, err)
	}
	return transactionReply(transaction), nil
}
