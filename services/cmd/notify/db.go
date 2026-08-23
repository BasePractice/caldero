package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"wish/services"
	"wish/services/shared/notify"

	"github.com/google/uuid"

	_ "github.com/lib/pq"
)

//go:embed migrations/*.sql
var migrations embed.FS

// ErrNotFound — записи нет. Отделена от сбоя БД: для обработчика это
// разные ответы клиенту.
var ErrNotFound = errors.New("not found")

// Task — задание на доставку, выданное воркеру.
type Task struct {
	Id       uuid.UUID
	EventId  uuid.UUID
	UserId   uuid.UUID
	Channel  notify.Channel
	Attempts int
	Type     notify.EventType
	Payload  map[string]string
	Created  time.Time
}

// TelegramBinding — привязка Telegram к пользователю.
type TelegramBinding struct {
	UserId  uuid.UUID
	ChatId  int64
	Blocked bool
	BoundAt time.Time
}

type Database interface {
	// Publish кладёт событие в очередь. Второй результат сообщает, что
	// событие с таким ключом дедупликации уже публиковалось.
	Publish(ctx context.Context, event notify.PublishEvent, channels []notify.Channel) (notify.Event, bool, error)
	// EnabledChannels возвращает каналы, включённые пользователем для типа
	// события. Каналы, которые пользователь выключил, не попадают в очередь
	// вовсе: отфильтровать их при доставке значило бы копить задания,
	// которые никогда не будут выполнены.
	EnabledChannels(ctx context.Context, user uuid.UUID, eventType notify.EventType) ([]notify.Channel, error)
	Preferences(ctx context.Context, user uuid.UUID) ([]notify.Preference, error)
	SetPreference(ctx context.Context, user uuid.UUID, preference notify.Preference) error

	// Claim арендует задания: сдвигает время следующей попытки, чтобы
	// их не взял другой воркер, и возвращает вместе с данными события.
	Claim(ctx context.Context, limit int, lease time.Duration) ([]Task, error)
	// Delivered отмечает задание выполненным.
	Delivered(ctx context.Context, id uuid.UUID) error
	// Retry откладывает задание после неудачи и считает попытку.
	Retry(ctx context.Context, id uuid.UUID, after time.Duration, reason string) error
	// Failed снимает задание с доставки окончательно.
	Failed(ctx context.Context, id uuid.UUID, reason string) error
	// Defer откладывает задание, не считая попытку: ограничение частоты —
	// это не отказ доставки, и тратить на него попытки нельзя.
	Defer(ctx context.Context, id uuid.UUID, after time.Duration) error
	// SentSince считает отправленное пользователю в канал за окно.
	SentSince(ctx context.Context, user uuid.UUID, channel notify.Channel, window time.Duration) (int, error)
	// Unsettled сообщает число заданий, ожидающих доставки. Нужен метрике:
	// растущая очередь — первый признак того, что канал не работает.
	Unsettled(ctx context.Context) (int, error)

	// AppendMessage кладёт сообщение в ленту приложения.
	AppendMessage(ctx context.Context, task Task, title, body string) (notify.Message, error)
	// Messages отдаёт ленту по курсору.
	Messages(ctx context.Context, user uuid.UUID, after int64, limit int) ([]notify.Message, error)

	// StartTelegramBinding заводит код привязки и возвращает его хеш.
	StartTelegramBinding(ctx context.Context, user uuid.UUID, codeHash []byte, expires time.Time) error
	// CompleteTelegramBinding связывает чат с пользователем по коду.
	CompleteTelegramBinding(ctx context.Context, codeHash []byte, chatId int64) (uuid.UUID, error)
	// TelegramBinding возвращает привязку пользователя.
	TelegramBinding(ctx context.Context, user uuid.UUID) (TelegramBinding, error)
	// BlockTelegram помечает бота заблокированным: слать в такой чат
	// бессмысленно, пока пользователь не разблокирует бота.
	BlockTelegram(ctx context.Context, user uuid.UUID) error

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
		return nil, fmt.Errorf("opening notify database: %w", err)
	}
	return &ds{db}, nil
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

func (d ds) Publish(
	ctx context.Context,
	publish notify.PublishEvent,
	channels []notify.Channel,
) (notify.Event, bool, error) {
	payload, err := json.Marshal(publish.Payload)
	if err != nil {
		return notify.Event{}, false, fmt.Errorf("encoding payload: %w", err)
	}

	var (
		event     notify.Event
		duplicate bool
	)
	err = d.inTx(ctx, func(tx *sql.Tx) error {
		var dedup any
		if publish.DedupKey != "" {
			dedup = publish.DedupKey
		}

		row := tx.QueryRowContext(ctx, `
			INSERT INTO event (user_id, type, payload, dedup_key)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (user_id, dedup_key) WHERE dedup_key IS NOT NULL DO NOTHING
			RETURNING id, created_at`, publish.UserId, publish.Type, payload, dedup)
		err := row.Scan(&event.Id, &event.CreatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			// Событие с таким ключом уже публиковалось: возвращаем то,
			// что уже есть, а не создаём второе сообщение.
			duplicate = true
			return tx.QueryRowContext(ctx, `
				SELECT id, created_at FROM event
				WHERE user_id = $1 AND dedup_key = $2`,
				publish.UserId, publish.DedupKey).Scan(&event.Id, &event.CreatedAt)
		}
		if err != nil {
			return fmt.Errorf("inserting event: %w", err)
		}

		for _, channel := range channels {
			if _, err = tx.ExecContext(ctx, `
				INSERT INTO delivery (event_id, user_id, channel)
				VALUES ($1, $2, $3)
				ON CONFLICT (event_id, channel) DO NOTHING`,
				event.Id, publish.UserId, channel); err != nil {
				return fmt.Errorf("queueing delivery to %s: %w", channel, err)
			}
		}
		return nil
	})
	if err != nil {
		return notify.Event{}, false, err
	}

	event.UserId = publish.UserId
	event.Type = publish.Type
	event.Payload = publish.Payload
	event.DedupKey = publish.DedupKey
	return event, duplicate, nil
}

func (d ds) EnabledChannels(
	ctx context.Context,
	user uuid.UUID,
	eventType notify.EventType,
) ([]notify.Channel, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT channel, enabled FROM preference
		WHERE user_id = $1 AND type = $2`, user, eventType)
	if err != nil {
		return nil, fmt.Errorf("loading preferences of user %s: %w", user, err)
	}
	defer func() {
		// Настоящая причина сбоя придёт из rows.Err().
		_ = rows.Close()
	}()

	configured := make(map[notify.Channel]bool)
	for rows.Next() {
		var channel notify.Channel
		var enabled bool
		if err = rows.Scan(&channel, &enabled); err != nil {
			return nil, fmt.Errorf("scanning preference: %w", err)
		}
		configured[channel] = enabled
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("reading preferences: %w", err)
	}

	enabled := make([]notify.Channel, 0, len(notify.Channels()))
	for _, channel := range notify.Channels() {
		on, ok := configured[channel]
		if !ok {
			on = notify.DefaultEnabled(eventType, channel)
		}
		if on {
			enabled = append(enabled, channel)
		}
	}
	return enabled, nil
}

func (d ds) Preferences(ctx context.Context, user uuid.UUID) ([]notify.Preference, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT type, channel, enabled FROM preference WHERE user_id = $1`, user)
	if err != nil {
		return nil, fmt.Errorf("loading preferences of user %s: %w", user, err)
	}
	defer func() {
		// Настоящая причина сбоя придёт из rows.Err().
		_ = rows.Close()
	}()

	configured := make(map[notify.Preference]bool)
	for rows.Next() {
		var preference notify.Preference
		if err = rows.Scan(&preference.Type, &preference.Channel, &preference.Enabled); err != nil {
			return nil, fmt.Errorf("scanning preference: %w", err)
		}
		configured[notify.Preference{Type: preference.Type, Channel: preference.Channel}] = preference.Enabled
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("reading preferences: %w", err)
	}

	// Отдаётся полная решётка «событие × канал»: клиенту нужно показать
	// все переключатели, а не только те, которых пользователь касался.
	preferences := make([]notify.Preference, 0, len(notify.EventTypes())*len(notify.Channels()))
	for _, eventType := range notify.EventTypes() {
		for _, channel := range notify.Channels() {
			key := notify.Preference{Type: eventType, Channel: channel}
			enabled, ok := configured[key]
			if !ok {
				enabled = notify.DefaultEnabled(eventType, channel)
			}
			preferences = append(preferences, notify.Preference{
				Type: eventType, Channel: channel, Enabled: enabled,
			})
		}
	}
	return preferences, nil
}

func (d ds) SetPreference(ctx context.Context, user uuid.UUID, preference notify.Preference) error {
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO preference (user_id, type, channel, enabled)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, type, channel)
		DO UPDATE SET enabled = EXCLUDED.enabled, updated_at = current_timestamp`,
		user, preference.Type, preference.Channel, preference.Enabled)
	if err != nil {
		return fmt.Errorf("saving preference %s/%s of user %s: %w",
			preference.Type, preference.Channel, user, err)
	}
	return nil
}

func (d ds) Claim(ctx context.Context, limit int, lease time.Duration) ([]Task, error) {
	// SKIP LOCKED пропускает строки, занятые другим воркером: без него
	// второй воркер ждал бы освобождения блокировки вместо того, чтобы
	// взять следующее задание.
	rows, err := d.db.QueryContext(ctx, `
		WITH claimed AS (
			SELECT id FROM delivery
			WHERE state = 'PENDING' AND next_attempt_at <= current_timestamp
			ORDER BY next_attempt_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		), leased AS (
			UPDATE delivery d
			SET next_attempt_at = current_timestamp + $2::interval
			FROM claimed
			WHERE d.id = claimed.id
			RETURNING d.id, d.event_id, d.user_id, d.channel, d.attempts
		)
		SELECT l.id, l.event_id, l.user_id, l.channel, l.attempts, e.type, e.payload, e.created_at
		FROM leased l JOIN event e ON e.id = l.event_id`,
		limit, fmt.Sprintf("%d seconds", int(lease.Seconds())))
	if err != nil {
		return nil, fmt.Errorf("claiming delivery tasks: %w", err)
	}
	defer func() {
		// Настоящая причина сбоя придёт из rows.Err().
		_ = rows.Close()
	}()

	var tasks []Task
	for rows.Next() {
		var task Task
		var payload []byte
		if err = rows.Scan(&task.Id, &task.EventId, &task.UserId, &task.Channel,
			&task.Attempts, &task.Type, &payload, &task.Created); err != nil {
			return nil, fmt.Errorf("scanning delivery task: %w", err)
		}
		if err = json.Unmarshal(payload, &task.Payload); err != nil {
			return nil, fmt.Errorf("decoding payload of event %s: %w", task.EventId, err)
		}
		tasks = append(tasks, task)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("reading delivery tasks: %w", err)
	}
	return tasks, nil
}

func (d ds) Delivered(ctx context.Context, id uuid.UUID) error {
	_, err := d.db.ExecContext(ctx, `
		UPDATE delivery
		SET state = 'DELIVERED', last_error = NULL, updated_at = current_timestamp
		WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("marking delivery %s delivered: %w", id, err)
	}
	return nil
}

func (d ds) Retry(ctx context.Context, id uuid.UUID, after time.Duration, reason string) error {
	_, err := d.db.ExecContext(ctx, `
		UPDATE delivery
		SET attempts = attempts + 1,
		    next_attempt_at = current_timestamp + $2::interval,
		    last_error = $3,
		    updated_at = current_timestamp
		WHERE id = $1`, id, fmt.Sprintf("%d seconds", int(after.Seconds())), truncate(reason))
	if err != nil {
		return fmt.Errorf("rescheduling delivery %s: %w", id, err)
	}
	return nil
}

func (d ds) Failed(ctx context.Context, id uuid.UUID, reason string) error {
	_, err := d.db.ExecContext(ctx, `
		UPDATE delivery
		SET state = 'FAILED', attempts = attempts + 1,
		    last_error = $2, updated_at = current_timestamp
		WHERE id = $1`, id, truncate(reason))
	if err != nil {
		return fmt.Errorf("marking delivery %s failed: %w", id, err)
	}
	return nil
}

func (d ds) Defer(ctx context.Context, id uuid.UUID, after time.Duration) error {
	_, err := d.db.ExecContext(ctx, `
		UPDATE delivery
		SET next_attempt_at = current_timestamp + $2::interval, updated_at = current_timestamp
		WHERE id = $1`, id, fmt.Sprintf("%d seconds", int(after.Seconds())))
	if err != nil {
		return fmt.Errorf("deferring delivery %s: %w", id, err)
	}
	return nil
}

func (d ds) SentSince(
	ctx context.Context,
	user uuid.UUID,
	channel notify.Channel,
	window time.Duration,
) (int, error) {
	var sent int
	err := d.db.QueryRowContext(ctx, `
		SELECT count(*) FROM delivery
		WHERE user_id = $1 AND channel = $2 AND state = 'DELIVERED'
		  AND updated_at > current_timestamp - $3::interval`,
		user, channel, fmt.Sprintf("%d seconds", int(window.Seconds()))).Scan(&sent)
	if err != nil {
		return 0, fmt.Errorf("counting deliveries to user %s: %w", user, err)
	}
	return sent, nil
}

func (d ds) Unsettled(ctx context.Context) (int, error) {
	var pending int
	if err := d.db.QueryRowContext(ctx, `
		SELECT count(*) FROM delivery WHERE state = 'PENDING'`).Scan(&pending); err != nil {
		return 0, fmt.Errorf("counting pending deliveries: %w", err)
	}
	return pending, nil
}

func (d ds) AppendMessage(ctx context.Context, task Task, title, body string) (notify.Message, error) {
	var message notify.Message
	err := d.inTx(ctx, func(tx *sql.Tx) error {
		// Сообщение уже могло быть создано предыдущей попыткой доставки,
		// прерванной после вставки.
		err := tx.QueryRowContext(ctx, `
			SELECT id, seq, type, title, body, created_at FROM message WHERE event_id = $1`,
			task.EventId).Scan(&message.Id, &message.Seq, &message.Type,
			&message.Title, &message.Body, &message.CreatedAt)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("looking up message of event %s: %w", task.EventId, err)
		}

		// Номер выдаётся строкой пользователя: обновление блокирует её
		// до конца транзакции, и одновременные вставки одному пользователю
		// получают номера строго по очереди.
		var seq int64
		if err = tx.QueryRowContext(ctx, `
			INSERT INTO message_sequence (user_id, last_seq) VALUES ($1, 1)
			ON CONFLICT (user_id) DO UPDATE SET last_seq = message_sequence.last_seq + 1
			RETURNING last_seq`, task.UserId).Scan(&seq); err != nil {
			return fmt.Errorf("allocating message sequence for user %s: %w", task.UserId, err)
		}

		if err = tx.QueryRowContext(ctx, `
			INSERT INTO message (user_id, seq, event_id, type, title, body)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING id, seq, type, title, body, created_at`,
			task.UserId, seq, task.EventId, task.Type, title, body).
			Scan(&message.Id, &message.Seq, &message.Type,
				&message.Title, &message.Body, &message.CreatedAt); err != nil {
			return fmt.Errorf("inserting message for event %s: %w", task.EventId, err)
		}
		return nil
	})
	if err != nil {
		return notify.Message{}, err
	}
	return message, nil
}

func (d ds) Messages(
	ctx context.Context,
	user uuid.UUID,
	after int64,
	limit int,
) ([]notify.Message, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, seq, type, title, body, created_at FROM message
		WHERE user_id = $1 AND seq > $2
		ORDER BY seq
		LIMIT $3`, user, after, limit)
	if err != nil {
		return nil, fmt.Errorf("loading messages of user %s: %w", user, err)
	}
	defer func() {
		// Настоящая причина сбоя придёт из rows.Err().
		_ = rows.Close()
	}()

	messages := make([]notify.Message, 0, limit)
	for rows.Next() {
		var message notify.Message
		if err = rows.Scan(&message.Id, &message.Seq, &message.Type,
			&message.Title, &message.Body, &message.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning message: %w", err)
		}
		messages = append(messages, message)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("reading messages: %w", err)
	}
	return messages, nil
}

func (d ds) StartTelegramBinding(
	ctx context.Context,
	user uuid.UUID,
	codeHash []byte,
	expires time.Time,
) error {
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO telegram_binding (user_id, code_hash, code_expires_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id) DO UPDATE
		SET code_hash = EXCLUDED.code_hash,
		    code_expires_at = EXCLUDED.code_expires_at,
		    updated_at = current_timestamp`, user, codeHash, expires)
	if err != nil {
		return fmt.Errorf("starting telegram binding for user %s: %w", user, err)
	}
	return nil
}

func (d ds) CompleteTelegramBinding(ctx context.Context, codeHash []byte, chatId int64) (uuid.UUID, error) {
	var user uuid.UUID
	err := d.db.QueryRowContext(ctx, `
		UPDATE telegram_binding
		SET chat_id = $2, bound_at = current_timestamp, blocked = FALSE,
		    code_hash = NULL, code_expires_at = NULL, updated_at = current_timestamp
		WHERE code_hash = $1 AND code_expires_at > current_timestamp
		RETURNING user_id`, codeHash, chatId).Scan(&user)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("completing telegram binding: %w", err)
	}
	return user, nil
}

func (d ds) TelegramBinding(ctx context.Context, user uuid.UUID) (TelegramBinding, error) {
	var binding TelegramBinding
	var chatId sql.NullInt64
	var boundAt sql.NullTime
	err := d.db.QueryRowContext(ctx, `
		SELECT user_id, chat_id, blocked, bound_at FROM telegram_binding WHERE user_id = $1`, user).
		Scan(&binding.UserId, &chatId, &binding.Blocked, &boundAt)
	if errors.Is(err, sql.ErrNoRows) {
		return TelegramBinding{}, ErrNotFound
	}
	if err != nil {
		return TelegramBinding{}, fmt.Errorf("loading telegram binding of user %s: %w", user, err)
	}
	if !chatId.Valid {
		// Привязка начата, но не завершена: чата ещё нет.
		return TelegramBinding{}, ErrNotFound
	}
	binding.ChatId = chatId.Int64
	if boundAt.Valid {
		binding.BoundAt = boundAt.Time
	}
	return binding, nil
}

func (d ds) BlockTelegram(ctx context.Context, user uuid.UUID) error {
	_, err := d.db.ExecContext(ctx, `
		UPDATE telegram_binding SET blocked = TRUE, updated_at = current_timestamp
		WHERE user_id = $1`, user)
	if err != nil {
		return fmt.Errorf("marking telegram blocked for user %s: %w", user, err)
	}
	return nil
}

func (d ds) Stats() sql.DBStats { return d.db.Stats() }

func (d ds) Ping(ctx context.Context) error { return d.db.PingContext(ctx) }

func (d ds) Close() error { return d.db.Close() }

// truncate укорачивает текст ошибки: он приходит от внешнего канала,
// и его длину задаёт не наш код.
func truncate(reason string) string {
	const limit = 500
	if len(reason) <= limit {
		return reason
	}
	return reason[:limit]
}
