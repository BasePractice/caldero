package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// defaultHistoryLimit и maxHistoryLimit ограничивают выдачу истории:
// без верхней границы один запрос вытянет всю таблицу.
const (
	defaultHistoryLimit = 50
	maxHistoryLimit     = 500

	// defaultReservationTTL — срок жизни резерва по умолчанию. Резерв без
	// срока блокирует средства навсегда, если подтверждение так и не придёт.
	defaultReservationTTL = 15 * time.Minute
)

type walletRow struct {
	id      uuid.UUID
	userId  uuid.UUID
	state   string
	balance int64
}

// inTx выполняет работу в транзакции. Откат делается в defer, потому что
// ошибка может прийти и из паники в теле.
func (d ds) inTx(ctx context.Context, work func(tx *sql.Tx) error) error {
	tx, err := d.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("starting transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			// Откат после успешного коммита вернул бы ErrTxDone —
			// поэтому он под условием, а его ошибка не важна.
			_ = tx.Rollback()
		}
	}()

	if err = work(tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}
	committed = true
	return nil
}

// lockWallet блокирует кошелёк владельца. Блокировка строки обязательна:
// без неё два одновременных списания оба прочитают старый баланс,
// и одно из них потеряется.
func lockWallet(ctx context.Context, tx *sql.Tx, owner uuid.UUID, walletId uuid.UUID) (walletRow, error) {
	if walletId != uuid.Nil {
		wallet, err := lockWalletById(ctx, tx, walletId)
		if err != nil {
			return walletRow{}, err
		}
		if wallet.userId != owner {
			// Тот же ответ, что и для несуществующего кошелька: иначе
			// перебором можно узнать, какие кошельки есть.
			return walletRow{}, ErrWalletNotFound
		}
		if wallet.state != "ACTIVE" {
			return walletRow{}, fmt.Errorf("%w: wallet is %s", ErrWalletNotActive, wallet.state)
		}
		return wallet, nil
	}

	// Кошелёк создаётся при первом обращении — как и при чтении информации.
	// Иначе первая же операция нового пользователя упирается в «нет кошелька»,
	// хотя требование прямо говорит о его автосоздании.
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO wallet (user_id) VALUES ($1) ON CONFLICT (user_id, type) DO NOTHING",
		owner); err != nil {
		return walletRow{}, fmt.Errorf("creating default wallet for user %s: %w", owner, err)
	}

	var wallet walletRow
	err := tx.QueryRowContext(ctx,
		`SELECT id, user_id, state, balance FROM wallet
		 WHERE user_id = $1 AND type = 'USER' FOR UPDATE`, owner).
		Scan(&wallet.id, &wallet.userId, &wallet.state, &wallet.balance)
	if errors.Is(err, sql.ErrNoRows) {
		return walletRow{}, ErrWalletNotFound
	}
	if err != nil {
		return walletRow{}, fmt.Errorf("locking wallet of user %s: %w", owner, err)
	}
	if wallet.state != "ACTIVE" {
		return walletRow{}, fmt.Errorf("%w: wallet is %s", ErrWalletNotActive, wallet.state)
	}
	return wallet, nil
}

func lockWalletById(ctx context.Context, tx *sql.Tx, walletId uuid.UUID) (walletRow, error) {
	var wallet walletRow
	err := tx.QueryRowContext(ctx,
		"SELECT id, user_id, state, balance FROM wallet WHERE id = $1 FOR UPDATE", walletId).
		Scan(&wallet.id, &wallet.userId, &wallet.state, &wallet.balance)
	if errors.Is(err, sql.ErrNoRows) {
		return walletRow{}, ErrWalletNotFound
	}
	if err != nil {
		return walletRow{}, fmt.Errorf("locking wallet %s: %w", walletId, err)
	}
	return wallet, nil
}

func findByIdempotencyKey(ctx context.Context, tx *sql.Tx, key string) (Transaction, bool, error) {
	if key == "" {
		return Transaction{}, false, nil
	}

	var t Transaction
	var source uuid.NullUUID
	var message sql.NullString
	err := tx.QueryRowContext(ctx,
		`SELECT t.id, t.target, t.source, t.operation, t.state, t.value, t.message, t.created_at, w.balance
		 FROM transaction t JOIN wallet w ON w.id = t.target
		 WHERE t.idempotency_key = $1`, key).
		Scan(&t.Id, &t.WalletId, &source, &t.Operation, &t.State, &t.Value, &message, &t.CreatedAt, &t.Balance)
	if errors.Is(err, sql.ErrNoRows) {
		return Transaction{}, false, nil
	}
	if err != nil {
		return Transaction{}, false, fmt.Errorf("looking up idempotency key: %w", err)
	}
	if source.Valid {
		t.SourceId = &source.UUID
	}
	t.Message = message.String
	return t, true, nil
}

type transactionInput struct {
	IdempotencyKey string
	WalletId       uuid.UUID
	SourceId       *uuid.UUID
	Operation      Operation
	Value          int64
	Message        string
	Balance        int64
}

// applyTransaction записывает транзакцию и сразу переводит её в SUCCESS,
// что запускает триггер изменения баланса. Промежуточное состояние RESERVED
// нужно только для операций с подтверждением — их пока нет.
func applyTransaction(ctx context.Context, tx *sql.Tx, input transactionInput) (Transaction, error) {
	var key any
	if input.IdempotencyKey != "" {
		key = input.IdempotencyKey
	}

	var t Transaction
	err := tx.QueryRowContext(ctx,
		`INSERT INTO transaction (target, source, operation, value, message, idempotency_key)
		 VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6)
		 RETURNING id, created_at`,
		input.WalletId, input.SourceId, string(input.Operation), input.Value, input.Message, key).
		Scan(&t.Id, &t.CreatedAt)
	if err != nil {
		return Transaction{}, fmt.Errorf("creating transaction: %w", err)
	}

	if _, err = tx.ExecContext(ctx,
		"UPDATE transaction SET state = 'SUCCESS', updated_at = now() WHERE id = $1 AND created_at = $2",
		t.Id, t.CreatedAt); err != nil {
		return Transaction{}, fmt.Errorf("confirming transaction %s: %w", t.Id, err)
	}

	t.WalletId = input.WalletId
	t.SourceId = input.SourceId
	t.Operation = input.Operation
	t.State = "SUCCESS"
	t.Value = input.Value
	t.Message = input.Message
	t.Balance = input.Balance
	return t, nil
}

// History отдаёт операции кошелька постранично. Курсором служит время
// создания: смещение через OFFSET на растущей таблице становится дороже
// с каждой страницей.
func (d ds) History(
	ctx context.Context,
	owner uuid.UUID,
	walletId uuid.UUID,
	limit int,
	before *time.Time,
) ([]Transaction, error) {
	if limit <= 0 {
		limit = defaultHistoryLimit
	}
	if limit > maxHistoryLimit {
		limit = maxHistoryLimit
	}

	query := `
SELECT t.id, t.target, t.source, t.operation, t.state, t.value, t.message, t.created_at, w.balance
FROM transaction t
         JOIN wallet w ON w.id = t.target
WHERE w.user_id = $1
  AND ($2::UUID IS NULL OR w.id = $2)
  AND ($3::TIMESTAMPTZ IS NULL OR t.created_at < $3)
ORDER BY t.created_at DESC
LIMIT $4`

	var wallet any
	if walletId != uuid.Nil {
		wallet = walletId
	}
	var cursor any
	if before != nil {
		cursor = *before
	}

	rows, err := d.db.QueryContext(ctx, query, owner, wallet, cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("querying history of user %s: %w", owner, err)
	}
	defer func() {
		// Настоящая причина сбоя придёт из rows.Err().
		_ = rows.Close()
	}()

	transactions := make([]Transaction, 0, limit)
	for rows.Next() {
		var t Transaction
		var source uuid.NullUUID
		var message sql.NullString
		if err = rows.Scan(&t.Id, &t.WalletId, &source, &t.Operation, &t.State,
			&t.Value, &message, &t.CreatedAt, &t.Balance); err != nil {
			return nil, fmt.Errorf("scanning transaction: %w", err)
		}
		if source.Valid {
			t.SourceId = &source.UUID
		}
		t.Message = message.String
		transactions = append(transactions, t)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("reading history of user %s: %w", owner, err)
	}
	return transactions, nil
}

// ChangeState меняет состояние кошелька. Допустимость перехода проверяет
// триггер: DELETED терминально.
func (d ds) ChangeState(ctx context.Context, owner, walletId uuid.UUID, state string) error {
	result, err := d.db.ExecContext(ctx,
		`UPDATE wallet SET state = $1, updated_at = now()
		 WHERE id = $2 AND user_id = $3`, state, walletId, owner)
	if err != nil {
		return fmt.Errorf("changing state of wallet %s: %w", walletId, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("changing state of wallet %s: %w", walletId, err)
	}
	if affected == 0 {
		return ErrWalletNotFound
	}
	return nil
}
