package main

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"

	"wish/services"
	"wish/services/shared/caldron"
	"wish/services/shared/credit"

	"github.com/google/uuid"

	_ "github.com/lib/pq"
)

//go:embed migrations/*.sql
var migrations embed.FS

var (
	// ErrNotFound — котла нет либо он не виден этому пользователю.
	ErrNotFound = errors.New("caldron not found")
	// ErrParticipantNotFound — пользователь не участвует в котле.
	ErrParticipantNotFound = errors.New("participant not found")
	// ErrAlreadyPaid — участник уже внёс средства.
	ErrAlreadyPaid = errors.New("participant has already contributed")
)

type Database interface {
	Create(ctx context.Context, create caldron.Caldron) (caldron.Caldron, error)
	// Caldron читает котёл вместе с участниками.
	Caldron(ctx context.Context, id uuid.UUID) (caldron.Caldron, error)
	// ByUser отдаёт котлы, где пользователь создатель или участник.
	ByUser(ctx context.Context, user uuid.UUID) ([]caldron.Caldron, error)
	// AddParticipant добавляет участника. Разрешено только создателю
	// и только в состоянии подготовки — это проверяется здесь же,
	// под блокировкой котла.
	AddParticipant(ctx context.Context, id uuid.UUID, participant caldron.AddParticipant) (caldron.Caldron, error)
	// RemoveParticipant убирает участника, который ещё не внёс средства.
	RemoveParticipant(ctx context.Context, id, user uuid.UUID) (caldron.Caldron, error)
	// SetWallet запоминает кошелёк котла.
	SetWallet(ctx context.Context, id, wallet uuid.UUID) error
	// StartContribution помечает намерение участника внести средства
	// и возвращает согласованную сумму: проверка режима и состояния идёт
	// под блокировкой, иначе два одновременных взноса пройдут оба.
	StartContribution(ctx context.Context, id, user uuid.UUID, requested credit.Amount) (caldron.Caldron, credit.Amount, error)
	// MarkPaid фиксирует внесённые средства и переводит котёл в «готов»,
	// если внесли все.
	MarkPaid(ctx context.Context, id, user uuid.UUID, amount credit.Amount) (caldron.Caldron, error)
	// MarkRefunded отмечает возврат средств участнику.
	MarkRefunded(ctx context.Context, id, user uuid.UUID) error
	// Transition меняет состояние котла с проверкой допустимости.
	Transition(ctx context.Context, id uuid.UUID, to caldron.State, actor caldron.Actor) (caldron.Caldron, error)
	// PendingRefunds отдаёт отменённые котлы, где кто-то ещё числится
	// внёсшим: возврат мог не завершиться из-за сбоя.
	PendingRefunds(ctx context.Context, limit int) ([]caldron.Caldron, error)

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
		return nil, fmt.Errorf("opening caldron database: %w", err)
	}
	return &ds{db}, nil
}

const caldronColumns = `id, creator_id, title, type, state, creator_participates, mode,
	amount, min_amount, max_amount, wallet_id, settled_at, cancelled_at, created_at, updated_at`

func scanCaldron(scanner interface{ Scan(...any) error }) (caldron.Caldron, error) {
	var (
		pot         caldron.Caldron
		amount      sql.NullInt64
		minAmount   sql.NullInt64
		maxAmount   sql.NullInt64
		walletId    sql.NullString
		settledAt   sql.NullTime
		cancelledAt sql.NullTime
	)
	if err := scanner.Scan(&pot.Id, &pot.CreatorId, &pot.Title, &pot.Type, &pot.State,
		&pot.CreatorParticipates, &pot.Mode, &amount, &minAmount, &maxAmount, &walletId,
		&settledAt, &cancelledAt, &pot.CreatedAt, &pot.UpdatedAt); err != nil {
		return caldron.Caldron{}, err
	}

	pot.Amount = credit.Amount(amount.Int64)
	pot.MinAmount = credit.Amount(minAmount.Int64)
	pot.MaxAmount = credit.Amount(maxAmount.Int64)
	if walletId.Valid {
		parsed, err := uuid.Parse(walletId.String)
		if err != nil {
			return caldron.Caldron{}, fmt.Errorf("parsing wallet id of caldron %s: %w", pot.Id, err)
		}
		pot.WalletId = &parsed
	}
	if settledAt.Valid {
		pot.SettledAt = &settledAt.Time
	}
	if cancelledAt.Valid {
		pot.CancelledAt = &cancelledAt.Time
	}
	return pot, nil
}

func (d ds) Create(ctx context.Context, create caldron.Caldron) (caldron.Caldron, error) {
	row := d.db.QueryRowContext(ctx, `
		INSERT INTO caldron (creator_id, title, type, state, creator_participates, mode,
		                     amount, min_amount, max_amount)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING `+caldronColumns,
		create.CreatorId, create.Title, create.Type, caldron.StatePreparing,
		create.CreatorParticipates, create.Mode,
		nullAmount(create.Amount), nullAmount(create.MinAmount), nullAmount(create.MaxAmount))

	pot, err := scanCaldron(row)
	if err != nil {
		return caldron.Caldron{}, fmt.Errorf("creating caldron for %s: %w", create.CreatorId, err)
	}
	return pot, nil
}

func (d ds) Caldron(ctx context.Context, id uuid.UUID) (caldron.Caldron, error) {
	pot, err := scanCaldron(d.db.QueryRowContext(ctx,
		`SELECT `+caldronColumns+` FROM caldron WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return caldron.Caldron{}, ErrNotFound
	}
	if err != nil {
		return caldron.Caldron{}, fmt.Errorf("loading caldron %s: %w", id, err)
	}

	if pot.Participants, pot.Collected, err = d.participants(ctx, d.db, id); err != nil {
		return caldron.Caldron{}, err
	}
	return pot, nil
}

// querier объединяет пул и транзакцию: участников читают и снаружи,
// и внутри транзакции перехода.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (d ds) participants(
	ctx context.Context,
	q querier,
	id uuid.UUID,
) ([]caldron.Participant, credit.Amount, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT caldron_id, user_id, expected, contributed, state, created_at, updated_at
		FROM participant WHERE caldron_id = $1 ORDER BY created_at`, id)
	if err != nil {
		return nil, 0, fmt.Errorf("loading participants of caldron %s: %w", id, err)
	}
	defer func() {
		// Настоящая причина сбоя придёт из rows.Err().
		_ = rows.Close()
	}()

	participants := make([]caldron.Participant, 0)
	var collected credit.Amount
	for rows.Next() {
		var (
			participant caldron.Participant
			expected    sql.NullInt64
			contributed int64
		)
		if err = rows.Scan(&participant.CaldronId, &participant.UserId, &expected, &contributed,
			&participant.State, &participant.CreatedAt, &participant.UpdatedAt); err != nil {
			return nil, 0, fmt.Errorf("scanning participant: %w", err)
		}
		participant.Expected = credit.Amount(expected.Int64)
		participant.Contributed = credit.Amount(contributed)
		if participant.State == caldron.ParticipantPaid {
			collected += participant.Contributed
		}
		participants = append(participants, participant)
	}
	if err = rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("reading participants: %w", err)
	}
	return participants, collected, nil
}

func (d ds) ByUser(ctx context.Context, user uuid.UUID) ([]caldron.Caldron, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT `+caldronColumns+`
		FROM caldron c
		WHERE c.creator_id = $1
		   OR EXISTS (SELECT 1 FROM participant p WHERE p.caldron_id = c.id AND p.user_id = $1)
		ORDER BY c.created_at DESC`, user)
	if err != nil {
		return nil, fmt.Errorf("loading caldrons of user %s: %w", user, err)
	}
	defer func() {
		// Настоящая причина сбоя придёт из rows.Err().
		_ = rows.Close()
	}()

	caldrons := make([]caldron.Caldron, 0)
	for rows.Next() {
		pot, err := scanCaldron(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning caldron: %w", err)
		}
		caldrons = append(caldrons, pot)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("reading caldrons: %w", err)
	}
	return caldrons, nil
}

func (d ds) AddParticipant(
	ctx context.Context,
	id uuid.UUID,
	add caldron.AddParticipant,
) (caldron.Caldron, error) {
	var updated caldron.Caldron
	err := d.inTx(ctx, func(tx *sql.Tx) error {
		pot, err := d.lock(ctx, tx, id)
		if err != nil {
			return err
		}
		// Добавление возможно только в подготовке: готовый котёл уже
		// собран, и новый участник сделал бы собранную сумму неверной.
		if pot.State != caldron.StatePreparing {
			return fmt.Errorf("%w: participants can be added only while the caldron is %s",
				caldron.ErrInvalidTransition, caldron.StatePreparing)
		}

		expected := add.Amount
		if pot.Mode == caldron.ModeFixed {
			expected = pot.Amount
		}
		if _, err = tx.ExecContext(ctx, `
			INSERT INTO participant (caldron_id, user_id, expected)
			VALUES ($1, $2, $3)
			ON CONFLICT (caldron_id, user_id) DO UPDATE
			SET expected = EXCLUDED.expected, updated_at = current_timestamp
			WHERE participant.state = 'INVITED'`,
			id, add.UserId, nullAmount(expected)); err != nil {
			return fmt.Errorf("adding participant %s: %w", add.UserId, err)
		}

		updated, err = d.reload(ctx, tx, id)
		return err
	})
	return updated, err
}

func (d ds) RemoveParticipant(ctx context.Context, id, user uuid.UUID) (caldron.Caldron, error) {
	var updated caldron.Caldron
	err := d.inTx(ctx, func(tx *sql.Tx) error {
		pot, err := d.lock(ctx, tx, id)
		if err != nil {
			return err
		}
		if pot.State != caldron.StatePreparing {
			return fmt.Errorf("%w: participants can be removed only while the caldron is %s",
				caldron.ErrInvalidTransition, caldron.StatePreparing)
		}

		// Внёсшего участника убрать нельзя: сначала возврат средств,
		// а он возможен только при отмене котла.
		result, err := tx.ExecContext(ctx, `
			DELETE FROM participant
			WHERE caldron_id = $1 AND user_id = $2 AND state = 'INVITED'`, id, user)
		if err != nil {
			return fmt.Errorf("removing participant %s: %w", user, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("counting removed participants: %w", err)
		}
		if affected == 0 {
			return ErrParticipantNotFound
		}

		updated, err = d.reload(ctx, tx, id)
		return err
	})
	return updated, err
}

func (d ds) SetWallet(ctx context.Context, id, wallet uuid.UUID) error {
	// Кошелёк присваивается один раз: смена кошелька у котла со средствами
	// означала бы потерю уже собранного.
	_, err := d.db.ExecContext(ctx, `
		UPDATE caldron SET wallet_id = $2, updated_at = current_timestamp
		WHERE id = $1 AND wallet_id IS NULL`, id, wallet)
	if err != nil {
		return fmt.Errorf("setting wallet of caldron %s: %w", id, err)
	}
	return nil
}

func (d ds) StartContribution(
	ctx context.Context,
	id, user uuid.UUID,
	requested credit.Amount,
) (caldron.Caldron, credit.Amount, error) {
	var (
		pot    caldron.Caldron
		amount credit.Amount
	)
	err := d.inTx(ctx, func(tx *sql.Tx) error {
		locked, err := d.lock(ctx, tx, id)
		if err != nil {
			return err
		}
		if locked.Participants, locked.Collected, err = d.participants(ctx, tx, id); err != nil {
			return err
		}

		var participant caldron.Participant
		found := false
		for _, candidate := range locked.Participants {
			if candidate.UserId == user {
				participant, found = candidate, true
				break
			}
		}
		if !found {
			return ErrParticipantNotFound
		}
		// Своё состояние проверяется раньше состояния котла: участнику,
		// который уже внёс, понятнее «вы уже внесли», чем «котёл собран».
		if participant.State != caldron.ParticipantInvited {
			return fmt.Errorf("%w: state is %s", ErrAlreadyPaid, participant.State)
		}
		if locked.State != caldron.StatePreparing {
			return fmt.Errorf("%w: caldron is %s", caldron.ErrInvalidTransition, locked.State)
		}

		if amount, err = locked.ContributionFor(participant, requested); err != nil {
			return err
		}
		pot = locked
		return nil
	})
	return pot, amount, err
}

func (d ds) MarkPaid(
	ctx context.Context,
	id, user uuid.UUID,
	amount credit.Amount,
) (caldron.Caldron, error) {
	var updated caldron.Caldron
	err := d.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := d.lock(ctx, tx, id); err != nil {
			return err
		}

		// Повторная отметка того же взноса ничего не меняет: средства
		// уже переведены, и второй записи быть не должно.
		if _, err := tx.ExecContext(ctx, `
			UPDATE participant
			SET state = 'PAID', contributed = $3, updated_at = current_timestamp
			WHERE caldron_id = $1 AND user_id = $2 AND state = 'INVITED'`,
			id, user, int64(amount)); err != nil {
			return fmt.Errorf("marking contribution of %s: %w", user, err)
		}

		reloaded, err := d.reload(ctx, tx, id)
		if err != nil {
			return err
		}
		// Котёл готов не по команде, а по факту: внесли все, от кого ждали.
		if reloaded.State == caldron.StatePreparing && reloaded.Complete() {
			if err = caldron.CanTransition(reloaded.State, caldron.StateReady, caldron.ActorSystem); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `
				UPDATE caldron SET state = $2, updated_at = current_timestamp WHERE id = $1`,
				id, caldron.StateReady); err != nil {
				return fmt.Errorf("marking caldron %s ready: %w", id, err)
			}
			if reloaded, err = d.reload(ctx, tx, id); err != nil {
				return err
			}
		}
		updated = reloaded
		return nil
	})
	return updated, err
}

func (d ds) MarkRefunded(ctx context.Context, id, user uuid.UUID) error {
	_, err := d.db.ExecContext(ctx, `
		UPDATE participant SET state = 'REFUNDED', updated_at = current_timestamp
		WHERE caldron_id = $1 AND user_id = $2 AND state = 'PAID'`, id, user)
	if err != nil {
		return fmt.Errorf("marking refund of %s: %w", user, err)
	}
	return nil
}

func (d ds) Transition(
	ctx context.Context,
	id uuid.UUID,
	to caldron.State,
	actor caldron.Actor,
) (caldron.Caldron, error) {
	var updated caldron.Caldron
	err := d.inTx(ctx, func(tx *sql.Tx) error {
		pot, err := d.lock(ctx, tx, id)
		if err != nil {
			return err
		}
		if err = caldron.CanTransition(pot.State, to, actor); err != nil {
			return err
		}

		if _, err = tx.ExecContext(ctx, `
			UPDATE caldron
			SET state = $2::VARCHAR,
			    settled_at = CASE WHEN $2::VARCHAR = 'SETTLED'
			                          THEN current_timestamp ELSE settled_at END,
			    cancelled_at = CASE WHEN $2::VARCHAR = 'CANCELLED'
			                            THEN current_timestamp ELSE cancelled_at END,
			    updated_at = current_timestamp
			WHERE id = $1`, id, string(to)); err != nil {
			return fmt.Errorf("changing state of caldron %s: %w", id, err)
		}

		updated, err = d.reload(ctx, tx, id)
		return err
	})
	return updated, err
}

func (d ds) PendingRefunds(ctx context.Context, limit int) ([]caldron.Caldron, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT `+caldronColumns+`
		FROM caldron c
		WHERE c.state = 'CANCELLED'
		  AND EXISTS (SELECT 1 FROM participant p WHERE p.caldron_id = c.id AND p.state = 'PAID')
		ORDER BY c.cancelled_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("loading caldrons with pending refunds: %w", err)
	}
	defer func() {
		// Настоящая причина сбоя придёт из rows.Err().
		_ = rows.Close()
	}()

	caldrons := make([]caldron.Caldron, 0)
	for rows.Next() {
		pot, err := scanCaldron(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning caldron: %w", err)
		}
		caldrons = append(caldrons, pot)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("reading caldrons: %w", err)
	}

	for i, pot := range caldrons {
		if caldrons[i].Participants, caldrons[i].Collected, err =
			d.participants(ctx, d.db, pot.Id); err != nil {
			return nil, err
		}
	}
	return caldrons, nil
}

// lock блокирует строку котла до конца транзакции: без этого два
// одновременных взноса оба увидят котёл незавершённым, а два одновременных
// перехода оба сочтут себя допустимыми.
func (d ds) lock(ctx context.Context, tx *sql.Tx, id uuid.UUID) (caldron.Caldron, error) {
	pot, err := scanCaldron(tx.QueryRowContext(ctx,
		`SELECT `+caldronColumns+` FROM caldron WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return caldron.Caldron{}, ErrNotFound
	}
	if err != nil {
		return caldron.Caldron{}, fmt.Errorf("locking caldron %s: %w", id, err)
	}
	return pot, nil
}

func (d ds) reload(ctx context.Context, tx *sql.Tx, id uuid.UUID) (caldron.Caldron, error) {
	pot, err := scanCaldron(tx.QueryRowContext(ctx,
		`SELECT `+caldronColumns+` FROM caldron WHERE id = $1`, id))
	if err != nil {
		return caldron.Caldron{}, fmt.Errorf("reloading caldron %s: %w", id, err)
	}
	if pot.Participants, pot.Collected, err = d.participants(ctx, tx, id); err != nil {
		return caldron.Caldron{}, err
	}
	return pot, nil
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

func nullAmount(amount credit.Amount) any {
	if amount == 0 {
		return nil
	}
	return int64(amount)
}
