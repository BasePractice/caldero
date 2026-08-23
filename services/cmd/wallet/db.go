package main

import (
	"database/sql"
	"embed"
	"fmt"

	"wish/middleware/wallet"
	"wish/services"

	"github.com/google/uuid"

	_ "github.com/lib/pq"
)

//go:embed migrations/*.sql
var migrations embed.FS

type DatabaseWallet interface {
	Information(userId uuid.UUID, cb func(reply *wallet.InformationReply)) error
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
func (d ds) Information(userId uuid.UUID, cb func(reply *wallet.InformationReply)) error {
	found, err := d.selectWallets(userId, cb)
	if err != nil {
		return err
	}
	if found {
		return nil
	}

	if err = d.ensureWallet(userId); err != nil {
		return err
	}
	if _, err = d.selectWallets(userId, cb); err != nil {
		return err
	}
	return nil
}

// ensureWallet создаёт кошелёк по умолчанию. ON CONFLICT обязателен: два
// параллельных запроса одного пользователя иначе нарушают UNIQUE (user_id, type),
// и второй запрос возвращает ошибку вместо кошелька.
func (d ds) ensureWallet(userId uuid.UUID) error {
	_, err := d.db.Exec(
		"INSERT INTO wallet (user_id) VALUES ($1) ON CONFLICT (user_id, type) DO NOTHING",
		userId)
	if err != nil {
		return fmt.Errorf("creating default wallet for user %s: %w", userId, err)
	}
	return nil
}

func (d ds) selectWallets(userId uuid.UUID, cb func(reply *wallet.InformationReply)) (bool, error) {
	rows, err := d.db.Query(informationQuery, userId)
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

func NewDatabaseWallet(cfg services.Config) (DatabaseWallet, error) {
	db, err := services.NewDatabase(cfg, migrations)
	if err != nil {
		return nil, fmt.Errorf("opening wallet database: %w", err)
	}
	return &ds{db}, nil
}
