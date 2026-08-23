package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"wish/services"
	"wish/services/shared/notify"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// busChannel — канал Redis, по которому экземпляры сервиса обмениваются
// новыми сообщениями.
const busChannel = "notify:messages"

// Bus доставляет сообщение подписчикам всех экземпляров сервиса.
//
// Без него WebSocket работал бы только там, где сообщение было создано:
// доставку выполняет тот экземпляр, который взял задание из очереди,
// а соединение пользователя держит другой. Длинный опрос в этом случае
// вырождается в опрос по таймауту — работать будет, но с задержкой
// в целый цикл ожидания.
type Bus struct {
	hub    *Hub
	client *redis.Client
}

// envelope — то, что уходит в Redis. Отдельный тип, потому что получателю
// нужен ещё и адресат, которого в самом сообщении нет.
type envelope struct {
	UserId  uuid.UUID      `json:"user_id"`
	Message notify.Message `json:"message"`
}

func NewBus(ctx context.Context, cfg services.Config, hub *Hub) (*Bus, error) {
	if cfg.RedisURL == "" {
		// Локальный режим: сообщения расходятся только подписчикам
		// этого экземпляра.
		return &Bus{hub: hub}, nil
	}

	options, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("parsing redis url: %w", err)
	}
	client := redis.NewClient(options)
	if err = client.Ping(ctx).Err(); err != nil {
		_ = client.Close() // Соединение всё равно не установлено.
		return nil, fmt.Errorf("connecting to redis: %w", err)
	}
	return &Bus{hub: hub, client: client}, nil
}

// Publish рассылает сообщение всем экземплярам. Локальные подписчики
// получают его через ту же подписку, а не отдельным путём: иначе при
// сбое Redis поведение экземпляров разошлось бы.
func (b *Bus) Publish(ctx context.Context, user uuid.UUID, message notify.Message) error {
	if b.client == nil {
		b.hub.Deliver(user, message)
		return nil
	}

	encoded, err := json.Marshal(envelope{UserId: user, Message: message})
	if err != nil {
		return fmt.Errorf("encoding message envelope: %w", err)
	}
	if err = b.client.Publish(ctx, busChannel, encoded).Err(); err != nil {
		// Сообщение уже в ленте, потеряна только мгновенная доставка:
		// раздаём хотя бы своим подписчикам и не считаем это отказом.
		b.hub.Deliver(user, message)
		return fmt.Errorf("publishing to redis: %w", err)
	}
	return nil
}

// Run разбирает подписку до отмены контекста.
func (b *Bus) Run(ctx context.Context) error {
	if b.client == nil {
		<-ctx.Done()
		return nil
	}

	subscription := b.client.Subscribe(ctx, busChannel)
	defer services.Close("bus subscription", subscription)

	messages := subscription.Channel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case received, ok := <-messages:
			if !ok {
				return errors.New("bus subscription closed")
			}
			var letter envelope
			if err := json.Unmarshal([]byte(received.Payload), &letter); err != nil {
				slog.WarnContext(ctx, "Malformed bus message", slog.String("err", err.Error()))
				continue
			}
			b.hub.Deliver(letter.UserId, letter.Message)
		}
	}
}

func (b *Bus) Close() error {
	if b.client == nil {
		return nil
	}
	return b.client.Close()
}
