package main

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"

	"wish/services"
	"wish/services/shared/credit"

	"github.com/google/uuid"

	_ "github.com/lib/pq"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Database interface {
	Create(ctx context.Context, credit credit.CreateCredit, operator *services.AuthorizedUser) (uuid.UUID, error)
	Get(ctx context.Context, id uuid.UUID) (*credit.Credit, error)
	// Close освобождает соединения с БД
	Close() error
	// Stats нужен для публикации метрик пула соединений.
	Stats() sql.DBStats
	RecordPayment(ctx context.Context, payment PaymentRecord) error

	// Ping нужен пробе готовности.
	Ping(ctx context.Context) error
}

type ds struct {
	db *sql.DB
}

// ErrCreditNotFound отделяет отсутствие записи от прочих ошибок БД:
// раньше обработчик отвечал 404 на любую ошибку, включая недоступную базу.
var ErrCreditNotFound = errors.New("credit not found")

func (d ds) Get(ctx context.Context, id uuid.UUID) (*credit.Credit, error) {
	var c credit.Credit
	var lastPaidAt sql.NullTime

	// already_payed, created_at и last_payed_at управляют расчётом графика:
	// без них частично погашенный кредит считался как только что выданный.
	err := d.db.QueryRowContext(ctx, `SELECT
		user_id, creator_id, type, rate_bp, balance, kind, month,
		already_paid, created_at, last_paid_at
		FROM credit WHERE id = $1`, id).
		Scan(&c.UserId, &c.CreatorId, &c.Type, &c.Rate, &c.Balance, &c.Kind, &c.Month,
			&c.AlreadyPaid, &c.CreatedAt, &lastPaidAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("credit %s: %w", id, ErrCreditNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("loading credit %s: %w", id, err)
	}
	if lastPaidAt.Valid {
		c.LastPaidAt = &lastPaidAt.Time
	}
	return &c, nil
}

func (d ds) Create(ctx context.Context, c credit.CreateCredit, operator *services.AuthorizedUser) (uuid.UUID, error) {
	var id uuid.UUID
	if err := d.db.QueryRowContext(ctx, `
		INSERT INTO credit (user_id, creator_id, type, rate_bp, balance, kind, month, already_paid)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
		c.UserId, operator.Id, c.Type, c.Rate, c.Balance, c.Kind, c.Month, c.AlreadyPaid).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("failed to create credit: %w", err)
	}
	return id, nil
}

func (d ds) Stats() sql.DBStats {
	return d.db.Stats()
}

// PaymentRecord — внесённый платёж по кредиту.
type PaymentRecord struct {
	CreditId       uuid.UUID
	IdempotencyKey string
	NeedValue      credit.Amount
	Amount         credit.Amount
}

// ErrPaymentAlreadyRecorded — платёж с таким ключом уже зафиксирован.
// Это не ошибка, а признак повтора: операция уже доведена до конца.
var ErrPaymentAlreadyRecorded = errors.New("payment already recorded")

// RecordPayment фиксирует платёж и увеличивает внесённую сумму кредита
// в одной транзакции: разошедшиеся между собой платёж и остаток
// по кредиту — это расхождение в деньгах.
func (d ds) RecordPayment(ctx context.Context, payment PaymentRecord) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting payment transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if _, err = tx.ExecContext(ctx, `
		INSERT INTO payment (credit_id, need_value, amount, payment_at, state, expired_at, idempotency_key)
		VALUES ($1, $2, $3, now(), 'COMPLETE', now(), $4)`,
		payment.CreditId, payment.NeedValue, payment.Amount, payment.IdempotencyKey); err != nil {
		if services.IsUniqueViolation(err) {
			return ErrPaymentAlreadyRecorded
		}
		return fmt.Errorf("recording payment for credit %s: %w", payment.CreditId, err)
	}

	if _, err = tx.ExecContext(ctx, `
		UPDATE credit SET already_paid = already_paid + $1, last_paid_at = now(), updated_at = now()
		WHERE id = $2`, payment.Amount, payment.CreditId); err != nil {
		return fmt.Errorf("updating credit %s: %w", payment.CreditId, err)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("committing payment: %w", err)
	}
	committed = true
	return nil
}

func (d ds) Ping(ctx context.Context) error {
	return d.db.PingContext(ctx)
}

func (d ds) Close() error {
	return d.db.Close()
}

func NewDatabase(ctx context.Context, cfg services.Config) (Database, error) {
	db, err := services.NewDatabase(ctx, cfg, migrations)
	if err != nil {
		return nil, fmt.Errorf("opening credit database: %w", err)
	}
	return &ds{db}, nil
}
