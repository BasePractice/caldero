package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"wish/services/shared/notify"
)

// Значения по умолчанию для доставки.
const (
	// defaultBatchSize — сколько заданий берётся за проход.
	defaultBatchSize = 32
	// defaultLease — на сколько задание уходит из выборки, пока его
	// обрабатывает воркер. Больше времени самой отправки: иначе второй
	// экземпляр возьмёт то же задание, пока первый ещё ждёт ответа канала.
	defaultLease = 2 * time.Minute
	// defaultIdle — пауза, когда очередь пуста.
	defaultIdle = 2 * time.Second
	// defaultMaxAttempts — после скольких неудач задание снимается
	// с доставки. Без предела недоступный канал копил бы повторы вечно.
	defaultMaxAttempts = 6
	// defaultRetryBase и defaultRetryMax задают рост задержки повторов.
	defaultRetryBase = 30 * time.Second
	defaultRetryMax  = 30 * time.Minute
	// defaultRateLimit и defaultRateWindow ограничивают частоту сообщений
	// одному пользователю в один канал. Без ограничения цепочка смен
	// состояния котла превращается в поток сообщений.
	defaultRateLimit  = 10
	defaultRateWindow = time.Minute
)

// Dispatcher разбирает очередь доставки.
//
// Внутри одного экземпляра задания обрабатываются последовательно:
// параллелизм даёт запуск нескольких экземпляров, а выборка с
// SKIP LOCKED следит, чтобы они не брали одно и то же задание.
type Dispatcher struct {
	db        Database
	templates *Templates
	senders   map[notify.Channel]Sender

	BatchSize   int
	Lease       time.Duration
	Idle        time.Duration
	MaxAttempts int
	RetryBase   time.Duration
	RetryMax    time.Duration
	RateLimit   int
	RateWindow  time.Duration
}

func NewDispatcher(db Database, templates *Templates, senders ...Sender) *Dispatcher {
	registry := make(map[notify.Channel]Sender, len(senders))
	for _, sender := range senders {
		registry[sender.Channel()] = sender
	}
	return &Dispatcher{
		db:          db,
		templates:   templates,
		senders:     registry,
		BatchSize:   defaultBatchSize,
		Lease:       defaultLease,
		Idle:        defaultIdle,
		MaxAttempts: defaultMaxAttempts,
		RetryBase:   defaultRetryBase,
		RetryMax:    defaultRetryMax,
		RateLimit:   defaultRateLimit,
		RateWindow:  defaultRateWindow,
	}
}

// Run разбирает очередь до отмены контекста.
func (d *Dispatcher) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}

		handled, err := d.Once(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.ErrorContext(ctx, "Delivery pass failed", slog.String("err", err.Error()))
		}
		if handled > 0 && err == nil {
			// Очередь не пуста: следующий проход без паузы.
			continue
		}

		timer := time.NewTimer(d.Idle)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// Once выполняет один проход и возвращает число обработанных заданий.
func (d *Dispatcher) Once(ctx context.Context) (int, error) {
	tasks, err := d.db.Claim(ctx, d.BatchSize, d.Lease)
	if err != nil {
		return 0, fmt.Errorf("claiming tasks: %w", err)
	}

	for _, task := range tasks {
		if ctx.Err() != nil {
			// Незавершённые задания вернутся в выборку сами, когда
			// истечёт аренда: терять их нельзя, а дорабатывать пачку
			// после сигнала остановки — незачем.
			return len(tasks), nil
		}
		d.handle(ctx, task)
	}
	return len(tasks), nil
}

func (d *Dispatcher) handle(ctx context.Context, task Task) {
	sender, ok := d.senders[task.Channel]
	if !ok {
		// Канал выключен в конфигурации — например, не задан токен бота.
		// Держать такое задание в очереди бессмысленно.
		d.settle(ctx, task, d.db.Failed(ctx, task.Id, "channel is not configured"))
		return
	}

	allowed, err := d.withinRate(ctx, task)
	if err != nil {
		slog.ErrorContext(ctx, "Can't check delivery rate",
			slog.String("task", task.Id.String()), slog.String("err", err.Error()))
		d.retry(ctx, task, err)
		return
	}
	if !allowed {
		// Превышение частоты — не отказ доставки: попытки на него
		// не тратятся, сообщение просто уходит на потом.
		pause := d.RateWindow / time.Duration(max(d.RateLimit, 1))
		if pause < time.Second {
			pause = time.Second
		}
		d.settle(ctx, task, d.db.Defer(ctx, task.Id, pause))
		return
	}

	title, body, err := d.templates.Render(task.Type, task.Payload)
	if err != nil {
		// Шаблон сам не починится: повторы ничего не изменят.
		slog.ErrorContext(ctx, "Can't render notification",
			slog.String("event", task.EventId.String()), slog.String("err", err.Error()))
		d.settle(ctx, task, d.db.Failed(ctx, task.Id, err.Error()))
		return
	}

	switch err = sender.Send(ctx, task, title, body); {
	case err == nil:
		d.settle(ctx, task, d.db.Delivered(ctx, task.Id))
		deliveries.WithLabelValues(string(task.Channel), "delivered").Inc()
	case Permanent(err):
		slog.InfoContext(ctx, "Delivery abandoned",
			slog.String("channel", string(task.Channel)),
			slog.String("user", task.UserId.String()),
			slog.String("reason", err.Error()))
		d.settle(ctx, task, d.db.Failed(ctx, task.Id, err.Error()))
		deliveries.WithLabelValues(string(task.Channel), "abandoned").Inc()
	default:
		d.retry(ctx, task, err)
	}
}

func (d *Dispatcher) retry(ctx context.Context, task Task, cause error) {
	if task.Attempts+1 >= d.MaxAttempts {
		slog.WarnContext(ctx, "Delivery failed permanently",
			slog.String("channel", string(task.Channel)),
			slog.Int("attempts", task.Attempts+1),
			slog.String("err", cause.Error()))
		d.settle(ctx, task, d.db.Failed(ctx, task.Id, cause.Error()))
		deliveries.WithLabelValues(string(task.Channel), "failed").Inc()
		return
	}

	delay := d.backoff(task.Attempts)
	slog.WarnContext(ctx, "Delivery postponed",
		slog.String("channel", string(task.Channel)),
		slog.Int("attempt", task.Attempts+1),
		slog.Duration("delay", delay),
		slog.String("err", cause.Error()))
	d.settle(ctx, task, d.db.Retry(ctx, task.Id, delay, cause.Error()))
	deliveries.WithLabelValues(string(task.Channel), "retried").Inc()
}

// settle логирует сбой отметки состояния. Само задание при этом не теряется:
// аренда истечёт, и оно вернётся в выборку.
func (d *Dispatcher) settle(ctx context.Context, task Task, err error) {
	if err != nil {
		slog.ErrorContext(ctx, "Can't update delivery state",
			slog.String("task", task.Id.String()), slog.String("err", err.Error()))
	}
}

func (d *Dispatcher) withinRate(ctx context.Context, task Task) (bool, error) {
	if d.RateLimit <= 0 {
		return true, nil
	}
	sent, err := d.db.SentSince(ctx, task.UserId, task.Channel, d.RateWindow)
	if err != nil {
		return false, err
	}
	return sent < d.RateLimit, nil
}

// backoff растит задержку экспоненциально: канал, который не отвечает
// сейчас, чаще всего не ответит и через секунду.
func (d *Dispatcher) backoff(attempts int) time.Duration {
	delay := time.Duration(float64(d.RetryBase) * math.Pow(2, float64(attempts)))
	if d.RetryMax > 0 && delay > d.RetryMax {
		delay = d.RetryMax
	}
	return delay
}

// ErrNoSenders — не подключён ни один канал доставки.
var ErrNoSenders = errors.New("no delivery channels configured")
