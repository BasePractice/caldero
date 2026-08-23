package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

var (
	// ErrReservationNotFound — резерва с таким идентификатором нет.
	ErrReservationNotFound = errors.New("reservation not found")
	// ErrReservationNotPending — резерв уже подтверждён или отменён.
	ErrReservationNotPending = errors.New("reservation is not pending")
)

// ReserveParams — параметры резервирования средств.
type ReserveParams struct {
	IdempotencyKey string
	WalletId       uuid.UUID
	Value          int64
	Message        string
	// TTL ограничивает время жизни резерва. Ноль означает значение
	// по умолчанию: резерв без срока блокирует средства навсегда.
	TTL time.Duration
}

// Reserve откладывает средства под будущее списание.
//
// Баланс не меняется: резерв уменьшает доступный остаток. Это существенно —
// пока операция не подтверждена, средства ещё принадлежат владельцу, но
// потратить их второй раз он не может.
func (d ds) Reserve(ctx context.Context, owner uuid.UUID, params ReserveParams) (Transaction, error) {
	if params.Value <= 0 {
		return Transaction{}, ErrInvalidValue
	}
	if params.TTL <= 0 {
		params.TTL = defaultReservationTTL
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

		wallet, err := lockWallet(ctx, tx, owner, params.WalletId)
		if err != nil {
			return err
		}

		available, err := availableBalance(ctx, tx, wallet.id, wallet.balance)
		if err != nil {
			return err
		}
		// Проверяется доступный остаток, а не баланс: иначе одни и те же
		// средства можно зарезервировать дважды.
		if available < params.Value {
			return fmt.Errorf("%w: available %d, requested %d",
				ErrInsufficientBalance, available, params.Value)
		}

		reservedUntil := time.Now().Add(params.TTL)
		var t Transaction
		err = tx.QueryRowContext(ctx,
			`INSERT INTO transaction (target, operation, value, message, idempotency_key, reserved_until)
			 VALUES ($1, 'CREDIT', $2, NULLIF($3, ''), NULLIF($4, ''), $5)
			 RETURNING id, created_at`,
			wallet.id, params.Value, params.Message, params.IdempotencyKey, reservedUntil).
			Scan(&t.Id, &t.CreatedAt)
		if err != nil {
			return fmt.Errorf("creating reservation: %w", err)
		}

		t.WalletId = wallet.id
		t.Operation = OperationCredit
		t.State = "RESERVED"
		t.Value = params.Value
		t.Message = params.Message
		t.Balance = wallet.balance
		result = t
		return nil
	})
	return result, err
}

// Confirm подтверждает резерв: средства списываются.
func (d ds) Confirm(ctx context.Context, owner, transactionId uuid.UUID) (Transaction, error) {
	return d.settle(ctx, owner, transactionId, "SUCCESS")
}

// Reject отменяет резерв: средства освобождаются.
func (d ds) Reject(ctx context.Context, owner, transactionId uuid.UUID) (Transaction, error) {
	return d.settle(ctx, owner, transactionId, "REJECTED")
}

func (d ds) settle(ctx context.Context, owner, transactionId uuid.UUID, state string) (Transaction, error) {
	var result Transaction
	err := d.inTx(ctx, func(tx *sql.Tx) error {
		var walletId uuid.UUID
		var current string
		var value int64
		var createdAt time.Time
		err := tx.QueryRowContext(ctx,
			`SELECT t.target, t.state, t.value, t.created_at
			 FROM transaction t JOIN wallet w ON w.id = t.target
			 WHERE t.id = $1 AND w.user_id = $2
			 FOR UPDATE OF t`, transactionId, owner).
			Scan(&walletId, &current, &value, &createdAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrReservationNotFound
		}
		if err != nil {
			return fmt.Errorf("loading reservation %s: %w", transactionId, err)
		}
		if current != "RESERVED" {
			return fmt.Errorf("%w: state is %s", ErrReservationNotPending, current)
		}

		// Кошелёк блокируется до изменения состояния: подтверждение резерва
		// меняет баланс через триггер, и без блокировки оно конкурирует
		// с обычными списаниями.
		wallet, err := lockWalletById(ctx, tx, walletId)
		if err != nil {
			return err
		}

		if _, err = tx.ExecContext(ctx,
			`UPDATE transaction SET state = $1, reserved_until = NULL, updated_at = now()
			 WHERE id = $2 AND created_at = $3`, state, transactionId, createdAt); err != nil {
			return fmt.Errorf("settling reservation %s: %w", transactionId, err)
		}

		balance := wallet.balance
		if state == "SUCCESS" {
			balance -= value
		}
		result = Transaction{
			Id: transactionId, WalletId: walletId, Operation: OperationCredit,
			State: state, Value: value, Balance: balance, CreatedAt: createdAt,
		}
		return nil
	})
	return result, err
}

// ReleaseExpiredReservations освобождает резервы с истёкшим сроком.
func (d ds) ReleaseExpiredReservations(ctx context.Context) (int64, error) {
	result, err := d.db.ExecContext(ctx,
		`UPDATE transaction SET state = 'REJECTED', reserved_until = NULL, updated_at = now()
		 WHERE state = 'RESERVED' AND reserved_until IS NOT NULL AND reserved_until < now()`)
	if err != nil {
		return 0, fmt.Errorf("releasing expired reservations: %w", err)
	}
	released, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("counting released reservations: %w", err)
	}
	return released, nil
}

// availableBalance — баланс за вычетом действующих резервов на списание.
func availableBalance(ctx context.Context, tx *sql.Tx, walletId uuid.UUID, balance int64) (int64, error) {
	var reserved int64
	err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(value), 0) FROM transaction
		 WHERE target = $1 AND state = 'RESERVED' AND operation = 'CREDIT'
		   AND (reserved_until IS NULL OR reserved_until > now())`, walletId).Scan(&reserved)
	if err != nil {
		return 0, fmt.Errorf("counting reserved amount of wallet %s: %w", walletId, err)
	}
	return balance - reserved, nil
}
