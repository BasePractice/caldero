package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"wish/services/shared/notify"
)

// Ошибки каналов. Разделены по тому, что с ними делать: одни имеет смысл
// повторить, другие не изменятся, сколько ни повторяй, и повторы только
// заняли бы очередь.
var (
	// ErrChannelUnavailable — временный сбой: сеть, таймаут, ответ 5xx.
	ErrChannelUnavailable = errors.New("channel is temporarily unavailable")
	// ErrChannelUnbound — канал не привязан к пользователю.
	ErrChannelUnbound = errors.New("channel is not linked to the user")
	// ErrChannelBlocked — пользователь заблокировал отправителя.
	ErrChannelBlocked = errors.New("channel is blocked by the user")
)

// Permanent сообщает, что повторять доставку бессмысленно.
func Permanent(err error) bool {
	return errors.Is(err, ErrChannelUnbound) || errors.Is(err, ErrChannelBlocked)
}

// Sender доставляет сообщение в один канал.
type Sender interface {
	Channel() notify.Channel
	Send(ctx context.Context, task Task, title, body string) error
}

// InApp кладёт сообщение в ленту приложения и раздаёт его активным
// сессиям. Лента — источник правды, рассылка — только мгновенная доставка:
// клиент, у которого не было соединения, заберёт сообщение по курсору.
type InApp struct {
	db  Database
	bus *Bus
}

func NewInApp(db Database, bus *Bus) *InApp {
	return &InApp{db: db, bus: bus}
}

func (i *InApp) Channel() notify.Channel { return notify.ChannelInApp }

func (i *InApp) Send(ctx context.Context, task Task, title, body string) error {
	message, err := i.db.AppendMessage(ctx, task, title, body)
	if err != nil {
		return fmt.Errorf("appending message: %w", err)
	}
	if err = i.bus.Publish(ctx, task.UserId, message); err != nil {
		// Сообщение уже в ленте: потеряна мгновенная доставка, а не
		// доставка вообще. Считать это отказом значило бы вставить
		// сообщение второй раз при повторе.
		slog.WarnContext(ctx, "Can't push message to active sessions",
			slog.String("message", message.Id.String()), slog.String("err", err.Error()))
	}
	return nil
}
