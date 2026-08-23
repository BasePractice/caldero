package main

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"time"

	wallet "wish/middleware/wallet/v1"
	"wish/services"

	"github.com/google/uuid"

	_ "github.com/lib/pq"
)

//go:embed migrations/*.sql
var migrations embed.FS

type DatabaseWallet interface {
	Information(ctx context.Context, userId uuid.UUID, cb func(reply *wallet.InformationReply)) error
	// Close освобождает соединения с БД
	Close() error
	Debit(ctx context.Context, owner uuid.UUID, params OperationParams) (Transaction, error)
	Credit(ctx context.Context, owner uuid.UUID, params OperationParams) (Transaction, error)
	Transfer(ctx context.Context, owner uuid.UUID, params TransferParams) (Transaction, error)
	History(ctx context.Context, owner, walletId uuid.UUID, limit int, before *time.Time) ([]Transaction, error)
	ChangeState(ctx context.Context, owner, walletId uuid.UUID, state string) error

	EnsurePartitions(ctx context.Context, monthsAhead int) (int, error)
	DefaultPartitionRows(ctx context.Context) (int64, error)

	// Stats нужен для публикации метрик пула соединений.
	Stats() sql.DBStats
	// Ping нужен пробе готовности.
	Ping(ctx context.Context) error
}

type ds struct {
	db *sql.DB
}

// informationQuery собирает по кошелькам пользователя баланс, число
// транзакций и суммы зарезервированных списаний и начислений.
//
// GROUP BY только по wlt.id: группировка ещё и по t.value разбивала агрегат
// по каждому номиналу, и один кошелёк возвращался несколько раз с частичными
// суммами. JOIN вместо LEFT JOIN: с внешним соединением сканировалась вся
// таблица transaction вместе с чужими транзакциями, которые всё равно
// отбрасывались следующим соединением.
const informationQuery = `
WITH wlt AS (SELECT *
             FROM wallet
             WHERE user_id = $1
               AND state <> 'DELETED'),
     trans_debit AS (SELECT SUM(t.value) AS value, wlt.id
                     FROM transaction t
                              JOIN wlt ON t.target = wlt.id
                     WHERE t.state = 'RESERVED'
                       AND t.operation = 'DEBIT'
                     GROUP BY wlt.id),
     trans_credit AS (SELECT SUM(t.value) AS value, wlt.id
                      FROM transaction t
                               JOIN wlt ON t.target = wlt.id
                      WHERE t.state = 'RESERVED'
                        AND t.operation = 'CREDIT'
                      GROUP BY wlt.id),
     trans AS (SELECT COUNT(t.id) AS count, wlt.id
               FROM transaction t
                        JOIN wlt ON t.target = wlt.id
               GROUP BY wlt.id)
SELECT wlt.id                          AS id,
       wlt.state                       AS state,
       wlt.type                        AS type,
       wlt.balance                     AS balance,
       COALESCE(trans.count, 0)        AS trans_c,
       COALESCE(trans_debit.value, 0)  AS dbt_value,
       COALESCE(trans_credit.value, 0) AS crd_value
FROM wlt
         LEFT JOIN trans ON wlt.id = trans.id
         LEFT JOIN trans_debit ON wlt.id = trans_debit.id
         LEFT JOIN trans_credit ON wlt.id = trans_credit.id
ORDER BY wlt.created_at;`

// Information отдаёт кошельки пользователя, создавая кошелёк по умолчанию,
// если его ещё нет.
func (d ds) Information(ctx context.Context, userId uuid.UUID, cb func(reply *wallet.InformationReply)) error {
	found, err := d.selectWallets(ctx, userId, cb)
	if err != nil {
		return err
	}
	if found {
		return nil
	}

	if err = d.ensureWallet(ctx, userId); err != nil {
		return err
	}
	if _, err = d.selectWallets(ctx, userId, cb); err != nil {
		return err
	}
	return nil
}

// ensureWallet создаёт кошелёк по умолчанию. ON CONFLICT обязателен: два
// параллельных запроса одного пользователя иначе нарушают UNIQUE (user_id, type),
// и второй запрос возвращает ошибку вместо кошелька.
func (d ds) ensureWallet(ctx context.Context, userId uuid.UUID) error {
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO wallet (user_id) VALUES ($1) ON CONFLICT (user_id, type) DO NOTHING",
		userId)
	if err != nil {
		return fmt.Errorf("creating default wallet for user %s: %w", userId, err)
	}
	return nil
}

func (d ds) selectWallets(ctx context.Context, userId uuid.UUID, cb func(reply *wallet.InformationReply)) (bool, error) {
	rows, err := d.db.QueryContext(ctx, informationQuery, userId)
	if err != nil {
		return false, fmt.Errorf("querying wallets of user %s: %w", userId, err)
	}
	defer func() {
		// Ошибка закрытия не влияет на уже прочитанные строки, а настоящая
		// причина сбоя приходит из rows.Err().
		_ = rows.Close()
	}()

	found := false
	for rows.Next() {
		var reply wallet.InformationReply
		var state, typ string
		if err = rows.Scan(
			&reply.Id, &state, &typ,
			&reply.Balance, &reply.Transactions,
			&reply.ReservedDebit, &reply.ReservedCredit); err != nil {
			return false, fmt.Errorf("scanning wallet of user %s: %w", userId, err)
		}
		reply.State = wallet.WalletState(wallet.WalletState_value[state])
		reply.Type = wallet.WalletType(wallet.WalletType_value[typ])
		found = true
		cb(&reply)
	}
	// Без этой проверки обрыв соединения посреди выборки выглядит как пустой
	// результат, и сервис молча пытается создать второй кошелёк.
	if err = rows.Err(); err != nil {
		return false, fmt.Errorf("reading wallets of user %s: %w", userId, err)
	}
	return found, nil
}

func (d ds) Stats() sql.DBStats {
	return d.db.Stats()
}

// EnsurePartitions создаёт партиции на monthsAhead месяцев вперёд.
// Возвращает число созданных: ноль — нормальная ситуация, значит окно
// ещё не кончилось.
func (d ds) EnsurePartitions(ctx context.Context, monthsAhead int) (int, error) {
	created := 0
	for month := range monthsAhead {
		var wasCreated bool
		err := d.db.QueryRowContext(ctx,
			"SELECT fn_ensure_transaction_partition((date_trunc('month', now()) + ($1 || ' months')::INTERVAL)::DATE)",
			month).Scan(&wasCreated)
		if err != nil {
			return created, fmt.Errorf("creating transaction partition for month +%d: %w", month, err)
		}
		if wasCreated {
			created++
		}
	}
	return created, nil
}

// DefaultPartitionRows считает строки, попавшие в партицию по умолчанию.
// Ненулевое значение означает, что окно партиций кончилось и это осталось
// незамеченным.
func (d ds) DefaultPartitionRows(ctx context.Context) (int64, error) {
	var rows int64
	if err := d.db.QueryRowContext(ctx, "SELECT count(*) FROM transaction_default").Scan(&rows); err != nil {
		return 0, fmt.Errorf("counting rows in default partition: %w", err)
	}
	return rows, nil
}

func (d ds) Ping(ctx context.Context) error {
	return d.db.PingContext(ctx)
}

func (d ds) Close() error {
	return d.db.Close()
}

func NewDatabaseWallet(ctx context.Context, cfg services.Config) (DatabaseWallet, error) {
	db, err := services.NewDatabase(ctx, cfg, migrations)
	if err != nil {
		return nil, fmt.Errorf("opening wallet database: %w", err)
	}
	return &ds{db}, nil
}
