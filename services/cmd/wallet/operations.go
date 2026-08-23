package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Ошибки операций. Отделены от ошибок БД, потому что каждая из них —
// это конкретный ответ клиенту, а не сбой сервиса.
var (
	ErrWalletNotFound      = errors.New("wallet not found")
	ErrWalletNotActive     = errors.New("wallet is not active")
	ErrInsufficientBalance = errors.New("insufficient balance")
	ErrSameWallet          = errors.New("source and target wallets are the same")
	ErrInvalidValue        = errors.New("value must be positive")
)

// Operation — направление движения средств.
type Operation string

const (
	OperationDebit  Operation = "DEBIT"
	OperationCredit Operation = "CREDIT"
	OperationSwap   Operation = "SWAP"
)

// Transaction — результат операции.
type Transaction struct {
	Id         uuid.UUID
	WalletId   uuid.UUID
	SourceId   *uuid.UUID
	Operation  Operation
	State      string
	Value      int64
	Balance    int64
	Message    string
	CreatedAt  time.Time
	Idempotent bool // Операция уже была выполнена ранее с тем же ключом.
}

// OperationParams — параметры зачисления или списания.
type OperationParams struct {
	IdempotencyKey string
	WalletId       uuid.UUID
	Value          int64
	Message        string
}

// TransferParams — параметры перевода.
type TransferParams struct {
	IdempotencyKey string
	SourceId       uuid.UUID
	TargetId       uuid.UUID
	Value          int64
	Message        string
}

// Debit зачисляет средства на кошелёк.
func (d ds) Debit(ctx context.Context, owner uuid.UUID, params OperationParams) (Transaction, error) {
	return d.operate(ctx, owner, params, OperationDebit)
}

// Credit списывает средства с кошелька.
func (d ds) Credit(ctx context.Context, owner uuid.UUID, params OperationParams) (Transaction, error) {
	return d.operate(ctx, owner, params, OperationCredit)
}

func (d ds) operate(
	ctx context.Context,
	owner uuid.UUID,
	params OperationParams,
	operation Operation,
) (Transaction, error) {
	if params.Value <= 0 {
		return Transaction{}, ErrInvalidValue
	}

	var result Transaction
	err := d.inTx(ctx, func(tx *sql.Tx) error {
		// Проверка идемпотентности внутри той же транзакции, что и запись:
		// иначе два одновременных повтора оба увидят «ключа нет».
		existing, found, err := findByIdempotencyKey(ctx, tx, params.IdempotencyKey)
		if err != nil {
			return err
		}
		if found {
			result = existing
			result.Idempotent = true
			return nil
		}

		wallet, err := lockWallet(ctx, tx, owner, params.WalletId)
		if err != nil {
			return err
		}
		balance := wallet.balance
		if operation == OperationDebit {
			balance += params.Value
		} else {
			// Проверяется доступный остаток, а не баланс: зарезервированные
			// средства ещё принадлежат владельцу, но потратить их второй раз
			// он не может.
			available, err := availableBalance(ctx, tx, wallet.id, wallet.balance)
			if err != nil {
				return err
			}
			if available < params.Value {
				return fmt.Errorf("%w: available %d, requested %d",
					ErrInsufficientBalance, available, params.Value)
			}
			balance -= params.Value
		}

		result, err = applyTransaction(ctx, tx, transactionInput{
			IdempotencyKey: params.IdempotencyKey,
			WalletId:       wallet.id,
			Operation:      operation,
			Value:          params.Value,
			Message:        params.Message,
			Balance:        balance,
		})
		return err
	})
	return result, err
}

// Transfer переводит средства между кошельками.
func (d ds) Transfer(ctx context.Context, owner uuid.UUID, params TransferParams) (Transaction, error) {
	if params.Value <= 0 {
		return Transaction{}, ErrInvalidValue
	}
	if params.SourceId == params.TargetId {
		return Transaction{}, ErrSameWallet
	}

	var result Transaction
	err := d.inTx(ctx, func(tx *sql.Tx) error {
		existing, found, err := findByIdempotencyKey(ctx, tx, params.IdempotencyKey)
		if err != nil {
			return err
		}
		if found {
			result = existing
			result.Idempotent = true
			return nil
		}

		// Кошельки блокируются в порядке идентификаторов: встречные переводы
		// иначедают взаимную блокировку.
		first, second := params.SourceId, params.TargetId
		if first.String() > second.String() {
			first, second = second, first
		}
		if _, err = lockWalletById(ctx, tx, first); err != nil {
			return err
		}
		if _, err = lockWalletById(ctx, tx, second); err != nil {
			return err
		}

		source, err := lockWalletById(ctx, tx, params.SourceId)
		if err != nil {
			return err
		}
		if source.userId != owner {
			return fmt.Errorf("%w: source wallet belongs to another user", ErrWalletNotFound)
		}
		available, err := availableBalance(ctx, tx, source.id, source.balance)
		if err != nil {
			return err
		}
		if available < params.Value {
			return fmt.Errorf("%w: available %d, requested %d",
				ErrInsufficientBalance, available, params.Value)
		}
		target, err := lockWalletById(ctx, tx, params.TargetId)
		if err != nil {
			return err
		}
		if target.state != "ACTIVE" {
			return fmt.Errorf("%w: target wallet is %s", ErrWalletNotActive, target.state)
		}

		sourceId := source.id
		result, err = applyTransaction(ctx, tx, transactionInput{
			IdempotencyKey: params.IdempotencyKey,
			WalletId:       target.id,
			SourceId:       &sourceId,
			Operation:      OperationSwap,
			Value:          params.Value,
			Message:        params.Message,
			Balance:        source.balance - params.Value,
		})
		return err
	})
	return result, err
}
