package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"wish/services"
	"wish/services/shared/credit"
	"wish/services/shared/marketplace"
	"wish/services/shared/wishlist"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

//go:embed migrations/*.sql
var migrations embed.FS

// ErrNotFound — элемента нет либо он не виден этому пользователю.
// Одна ошибка на оба случая намеренно: различие подсказывало бы, что
// элемент существует, и позволяло бы перебирать чужие списки.
var ErrNotFound = errors.New("item not found")

// ErrStateChanged — состояние элемента изменилось между чтением и записью.
var ErrStateChanged = errors.New("item state has changed")

// Transition — описание перехода: кто, куда и что при этом меняется.
type Transition struct {
	Actor wishlist.Actor
	To    wishlist.State
	// Giver проставляется при резервировании и снимается при возврате.
	Giver *uuid.UUID
	// ReservedUntil действует только в состоянии CHOSEN.
	ReservedUntil *time.Time
	// OrderId заполняется при акцепте товара, если заказ удалось оформить.
	OrderId string
}

type Database interface {
	Create(ctx context.Context, item wishlist.Item) (wishlist.Item, error)
	// Items отдаёт список пользователя. Для чужого списка states
	// ограничивает выборку видимыми элементами.
	Items(ctx context.Context, owner uuid.UUID, states []wishlist.State) ([]wishlist.Item, error)
	// Chosen отдаёт то, что выбрал даритель.
	Chosen(ctx context.Context, giver uuid.UUID) ([]wishlist.Item, error)
	Item(ctx context.Context, id uuid.UUID) (wishlist.Item, error)
	Delete(ctx context.Context, id uuid.UUID, owner uuid.UUID) error
	// Transition меняет состояние элемента, проверив допустимость перехода
	// под блокировкой строки: без неё два дарителя резервируют один подарок.
	Transition(ctx context.Context, id uuid.UUID, transition Transition) (wishlist.Item, error)
	// ReleaseExpired возвращает в список подарки с просроченным резервом.
	ReleaseExpired(ctx context.Context) ([]wishlist.Item, error)

	// Шопоголик: прогон, отобранные товары и их судьба.
	StartRun(ctx context.Context, user uuid.UUID, budget credit.Amount, seed []byte) (wishlist.Run, error)
	AddPurchase(ctx context.Context, purchase wishlist.Purchase) (wishlist.Purchase, error)
	SettlePurchase(ctx context.Context, id uuid.UUID, ordered, paid bool, orderId, failure string) error
	FinishRun(ctx context.Context, id uuid.UUID, spent credit.Amount, state wishlist.RunState) (wishlist.Run, error)
	Run(ctx context.Context, id uuid.UUID) (wishlist.Run, error)
	Runs(ctx context.Context, user uuid.UUID, limit int) ([]wishlist.Run, error)

	Close() error
	Stats() sql.DBStats
	Ping(ctx context.Context) error
}

type ds struct {
	db *sql.DB
}

func NewDatabase(ctx context.Context, cfg services.Config) (Database, error) {
	db, err := services.NewDatabase(ctx, cfg, migrations)
	if err != nil {
		return nil, fmt.Errorf("opening wishlist database: %w", err)
	}
	return &ds{db}, nil
}

const itemColumns = `id, user_id, kind, state, priority, title, comment,
	provider, product_id, url, price, price_at, amount,
	giver_id, reserved_until, order_id, created_at, updated_at`

func scanItem(scanner interface{ Scan(...any) error }) (wishlist.Item, error) {
	var (
		item          wishlist.Item
		comment       sql.NullString
		provider      sql.NullString
		productId     sql.NullString
		url           sql.NullString
		price         sql.NullInt64
		priceAt       sql.NullTime
		amount        sql.NullInt64
		giver         sql.NullString
		reservedUntil sql.NullTime
		orderId       sql.NullString
	)
	if err := scanner.Scan(&item.Id, &item.UserId, &item.Kind, &item.State, &item.Priority,
		&item.Title, &comment, &provider, &productId, &url, &price, &priceAt, &amount,
		&giver, &reservedUntil, &orderId, &item.CreatedAt, &item.UpdatedAt); err != nil {
		return wishlist.Item{}, err
	}

	item.Comment = comment.String
	item.Provider = marketplaceProvider(provider.String)
	item.ProductId = productId.String
	item.URL = url.String
	item.Price = amountOf(price)
	if priceAt.Valid {
		item.PriceAt = &priceAt.Time
	}
	item.Amount = amountOf(amount)
	if giver.Valid {
		parsed, err := uuid.Parse(giver.String)
		if err != nil {
			return wishlist.Item{}, fmt.Errorf("parsing giver id of item %s: %w", item.Id, err)
		}
		item.GiverId = &parsed
	}
	if reservedUntil.Valid {
		item.ReservedUntil = &reservedUntil.Time
	}
	item.OrderId = orderId.String
	return item, nil
}

func (d ds) Create(ctx context.Context, item wishlist.Item) (wishlist.Item, error) {
	row := d.db.QueryRowContext(ctx, `
		INSERT INTO item (user_id, kind, state, priority, title, comment,
		                  provider, product_id, url, price, price_at, amount)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''),
		        NULLIF($7, ''), NULLIF($8, ''), NULLIF($9, ''), $10, $11, $12)
		RETURNING `+itemColumns,
		item.UserId, item.Kind, item.State, item.Priority, item.Title, item.Comment,
		string(item.Provider), item.ProductId, item.URL,
		nullAmount(item.Price), nullTime(item.PriceAt), nullAmount(item.Amount))

	created, err := scanItem(row)
	if err != nil {
		return wishlist.Item{}, fmt.Errorf("creating wishlist item for user %s: %w", item.UserId, err)
	}
	return created, nil
}

func (d ds) Items(
	ctx context.Context,
	owner uuid.UUID,
	states []wishlist.State,
) ([]wishlist.Item, error) {
	filter := make([]string, 0, len(states))
	for _, state := range states {
		filter = append(filter, string(state))
	}

	// Пустой список состояний означает «все»: массив в запросе один,
	// чтобы не собирать SQL строками.
	rows, err := d.db.QueryContext(ctx, `
		SELECT `+itemColumns+`
		FROM item
		WHERE user_id = $1
		  AND (cardinality($2::VARCHAR[]) = 0 OR state = ANY ($2::VARCHAR[]))
		ORDER BY priority, created_at`, owner, pq.Array(filter))
	if err != nil {
		return nil, fmt.Errorf("loading wishlist of user %s: %w", owner, err)
	}
	return collectItems(rows)
}

func (d ds) Chosen(ctx context.Context, giver uuid.UUID) ([]wishlist.Item, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT `+itemColumns+`
		FROM item
		WHERE giver_id = $1 AND state IN ('CHOSEN', 'CONFIRMED', 'ACCEPTED')
		ORDER BY updated_at DESC`, giver)
	if err != nil {
		return nil, fmt.Errorf("loading items chosen by %s: %w", giver, err)
	}
	return collectItems(rows)
}

func collectItems(rows *sql.Rows) ([]wishlist.Item, error) {
	defer func() {
		// Настоящая причина сбоя придёт из rows.Err().
		_ = rows.Close()
	}()

	items := make([]wishlist.Item, 0)
	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning wishlist item: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading wishlist items: %w", err)
	}
	return items, nil
}

func (d ds) Item(ctx context.Context, id uuid.UUID) (wishlist.Item, error) {
	item, err := scanItem(d.db.QueryRowContext(ctx, `
		SELECT `+itemColumns+` FROM item WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return wishlist.Item{}, ErrNotFound
	}
	if err != nil {
		return wishlist.Item{}, fmt.Errorf("loading item %s: %w", id, err)
	}
	return item, nil
}

func (d ds) Delete(ctx context.Context, id uuid.UUID, owner uuid.UUID) error {
	// Удаляется только то, что никто не выбрал: иначе даритель узнал бы
	// об отказе исчезновением подарка, а не оповещением.
	result, err := d.db.ExecContext(ctx, `
		DELETE FROM item
		WHERE id = $1 AND user_id = $2 AND state IN ('VISIBLE', 'HIDDEN')`, id, owner)
	if err != nil {
		return fmt.Errorf("deleting item %s: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("counting deleted items: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (d ds) Transition(
	ctx context.Context,
	id uuid.UUID,
	transition Transition,
) (wishlist.Item, error) {
	var updated wishlist.Item
	err := d.inTx(ctx, func(tx *sql.Tx) error {
		// FOR UPDATE держит строку до конца транзакции: два дарителя,
		// одновременно выбравшие один подарок, проходят по очереди,
		// и второй видит состояние, изменённое первым.
		current, err := scanItem(tx.QueryRowContext(ctx, `
			SELECT `+itemColumns+` FROM item WHERE id = $1 FOR UPDATE`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("locking item %s: %w", id, err)
		}

		if err = wishlist.CanTransition(current.State, transition.To, transition.Actor); err != nil {
			return err
		}
		// Право дарителя проверяется здесь же: резерв принадлежит тому,
		// кто его поставил, а не любому, кто знает идентификатор.
		if transition.Actor == wishlist.ActorGiver && current.GiverId != nil &&
			transition.Giver != nil && *current.GiverId != *transition.Giver {
			return fmt.Errorf("%w: item is reserved by another giver", wishlist.ErrForbiddenTransition)
		}

		giver := transition.Giver
		if transition.To == wishlist.StateVisible || transition.To == wishlist.StateHidden {
			// Резерв снят: подарок снова ничей.
			giver = nil
		} else if giver == nil {
			giver = current.GiverId
		}

		orderId := transition.OrderId
		if orderId == "" {
			orderId = current.OrderId
		}

		updated, err = scanItem(tx.QueryRowContext(ctx, `
			UPDATE item
			SET state = $2, giver_id = $3, reserved_until = $4,
			    order_id = NULLIF($5, ''), updated_at = current_timestamp
			WHERE id = $1
			RETURNING `+itemColumns,
			id, transition.To, nullUUID(giver), nullTime(transition.ReservedUntil), orderId))
		if err != nil {
			return fmt.Errorf("updating item %s: %w", id, err)
		}
		return nil
	})
	if err != nil {
		return wishlist.Item{}, err
	}
	return updated, nil
}

func (d ds) ReleaseExpired(ctx context.Context) ([]wishlist.Item, error) {
	rows, err := d.db.QueryContext(ctx, `
		UPDATE item
		SET state = 'VISIBLE', giver_id = NULL, reserved_until = NULL,
		    updated_at = current_timestamp
		WHERE state = 'CHOSEN' AND reserved_until <= current_timestamp
		RETURNING `+itemColumns)
	if err != nil {
		return nil, fmt.Errorf("releasing expired reservations: %w", err)
	}
	return collectItems(rows)
}

func (d ds) inTx(ctx context.Context, do func(tx *sql.Tx) error) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting transaction: %w", err)
	}
	defer func() {
		// Откат после успешной фиксации возвращает ErrTxDone и ничего
		// не меняет, поэтому проверять его нечего.
		_ = tx.Rollback()
	}()

	if err = do(tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}
	return nil
}

func (d ds) Stats() sql.DBStats { return d.db.Stats() }

func (d ds) Ping(ctx context.Context) error { return d.db.PingContext(ctx) }

func (d ds) Close() error { return d.db.Close() }

// StartRun заводит прогон шопоголика. Прогон создаётся до обращений
// к площадке и кошельку: если что-то пойдёт не так, останется запись,
// по которой видно, что происходило.
func (d ds) StartRun(
	ctx context.Context,
	user uuid.UUID,
	budget credit.Amount,
	seed []byte,
) (wishlist.Run, error) {
	run, err := scanRun(d.db.QueryRowContext(ctx, `
		INSERT INTO shopping_run (user_id, budget, seed)
		VALUES ($1, $2, $3)
		RETURNING id, user_id, budget, spent, state, seed, created_at`,
		user, int64(budget), seed))
	if err != nil {
		return wishlist.Run{}, fmt.Errorf("starting shopping run for %s: %w", user, err)
	}
	return run, nil
}

// AddPurchase записывает отобранный товар.
func (d ds) AddPurchase(ctx context.Context, purchase wishlist.Purchase) (wishlist.Purchase, error) {
	saved, err := scanPurchase(d.db.QueryRowContext(ctx, `
		INSERT INTO purchase (run_id, provider, product_id, title, url, price)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6)
		RETURNING id, run_id, provider, product_id, title, COALESCE(url, ''),
		          price, ordered, paid, COALESCE(order_id, ''), COALESCE(failure, ''), created_at`,
		purchase.RunId, string(purchase.Provider), purchase.ProductId,
		purchase.Title, purchase.URL, int64(purchase.Price)))
	if err != nil {
		return wishlist.Purchase{}, fmt.Errorf("adding purchase %s: %w", purchase.ProductId, err)
	}
	return saved, nil
}

// SettlePurchase отмечает судьбу товара: заказан ли он и оплачен ли.
func (d ds) SettlePurchase(
	ctx context.Context,
	id uuid.UUID,
	ordered, paid bool,
	orderId, failure string,
) error {
	_, err := d.db.ExecContext(ctx, `
		UPDATE purchase
		SET ordered = $2, paid = $3, order_id = NULLIF($4, ''), failure = NULLIF($5, '')
		WHERE id = $1`, id, ordered, paid, orderId, truncateFailure(failure))
	if err != nil {
		return fmt.Errorf("settling purchase %s: %w", id, err)
	}
	return nil
}

// FinishRun записывает итог прогона.
func (d ds) FinishRun(
	ctx context.Context,
	id uuid.UUID,
	spent credit.Amount,
	state wishlist.RunState,
) (wishlist.Run, error) {
	run, err := scanRun(d.db.QueryRowContext(ctx, `
		UPDATE shopping_run SET spent = $2, state = $3
		WHERE id = $1
		RETURNING id, user_id, budget, spent, state, seed, created_at`,
		id, int64(spent), state))
	if err != nil {
		return wishlist.Run{}, fmt.Errorf("finishing shopping run %s: %w", id, err)
	}

	if run.Purchases, err = d.purchases(ctx, id); err != nil {
		return wishlist.Run{}, err
	}
	return run, nil
}

// Run читает прогон вместе с покупками.
func (d ds) Run(ctx context.Context, id uuid.UUID) (wishlist.Run, error) {
	run, err := scanRun(d.db.QueryRowContext(ctx, `
		SELECT id, user_id, budget, spent, state, seed, created_at
		FROM shopping_run WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return wishlist.Run{}, ErrNotFound
	}
	if err != nil {
		return wishlist.Run{}, fmt.Errorf("loading shopping run %s: %w", id, err)
	}

	if run.Purchases, err = d.purchases(ctx, id); err != nil {
		return wishlist.Run{}, err
	}
	return run, nil
}

// Runs отдаёт историю прогонов пользователя.
func (d ds) Runs(ctx context.Context, user uuid.UUID, limit int) ([]wishlist.Run, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, user_id, budget, spent, state, seed, created_at
		FROM shopping_run WHERE user_id = $1
		ORDER BY created_at DESC
		LIMIT $2`, user, limit)
	if err != nil {
		return nil, fmt.Errorf("loading shopping runs of %s: %w", user, err)
	}
	defer func() {
		// Настоящая причина сбоя придёт из rows.Err().
		_ = rows.Close()
	}()

	runs := make([]wishlist.Run, 0)
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning shopping run: %w", err)
		}
		runs = append(runs, run)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("reading shopping runs: %w", err)
	}
	return runs, nil
}

func (d ds) purchases(ctx context.Context, run uuid.UUID) ([]wishlist.Purchase, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, run_id, provider, product_id, title, COALESCE(url, ''),
		       price, ordered, paid, COALESCE(order_id, ''), COALESCE(failure, ''), created_at
		FROM purchase WHERE run_id = $1 ORDER BY created_at`, run)
	if err != nil {
		return nil, fmt.Errorf("loading purchases of run %s: %w", run, err)
	}
	defer func() {
		// Настоящая причина сбоя придёт из rows.Err().
		_ = rows.Close()
	}()

	purchases := make([]wishlist.Purchase, 0)
	for rows.Next() {
		purchase, err := scanPurchase(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning purchase: %w", err)
		}
		purchases = append(purchases, purchase)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("reading purchases: %w", err)
	}
	return purchases, nil
}

func scanRun(scanner interface{ Scan(...any) error }) (wishlist.Run, error) {
	var (
		run    wishlist.Run
		budget int64
		spent  int64
		seed   []byte
	)
	if err := scanner.Scan(&run.Id, &run.UserId, &budget, &spent,
		&run.State, &seed, &run.CreatedAt); err != nil {
		return wishlist.Run{}, err
	}
	run.Budget = credit.Amount(budget)
	run.Spent = credit.Amount(spent)
	run.Seed = hex.EncodeToString(seed)
	return run, nil
}

func scanPurchase(scanner interface{ Scan(...any) error }) (wishlist.Purchase, error) {
	var (
		purchase wishlist.Purchase
		provider string
		price    int64
	)
	if err := scanner.Scan(&purchase.Id, &purchase.RunId, &provider, &purchase.ProductId,
		&purchase.Title, &purchase.URL, &price, &purchase.Ordered, &purchase.Paid,
		&purchase.OrderId, &purchase.Failure, &purchase.CreatedAt); err != nil {
		return wishlist.Purchase{}, err
	}
	purchase.Provider = marketplace.Provider(provider)
	purchase.Price = credit.Amount(price)
	return purchase, nil
}

// truncateFailure укорачивает текст ошибки: он приходит от площадки,
// и его длину задаёт не наш код.
func truncateFailure(failure string) string {
	const limit = 300
	if len(failure) <= limit {
		return failure
	}
	return failure[:limit]
}
