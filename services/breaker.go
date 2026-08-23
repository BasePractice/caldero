package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// ErrCircuitOpen возвращается, пока размыкатель разомкнут.
var ErrCircuitOpen = errors.New("circuit is open")

// BreakerState — состояние размыкателя.
type BreakerState int

const (
	// BreakerClosed — вызовы проходят.
	BreakerClosed BreakerState = iota
	// BreakerOpen — вызовы отклоняются сразу.
	BreakerOpen
	// BreakerHalfOpen — пропускается пробный вызов.
	BreakerHalfOpen
)

func (s BreakerState) String() string {
	switch s {
	case BreakerClosed:
		return "closed"
	case BreakerOpen:
		return "open"
	case BreakerHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// Breaker размыкает цепь после серии отказов.
//
// Смысл не в том, чтобы вызывающий получил ошибку быстрее, а в том, чтобы
// перестать добивать упавшую зависимость: она восстанавливается тем дольше,
// чем больше запросов продолжает получать. Побочный эффект — вызывающий
// не ждёт таймаута на каждом запросе.
type Breaker struct {
	name       string
	threshold  int
	resetAfter time.Duration
	isFailure  func(error) bool

	mu          sync.Mutex
	state       BreakerState
	failures    int
	lastFailure time.Time
}

// NewBreaker создаёт размыкатель. isFailure отделяет отказы зависимости
// от обычных ошибок: «товар не найден» не повод размыкать цепь.
func NewBreaker(name string, threshold int, resetAfter time.Duration, isFailure func(error) bool) *Breaker {
	if threshold < 1 {
		threshold = 1
	}
	if isFailure == nil {
		isFailure = func(error) bool { return true }
	}
	return &Breaker{
		name: name, threshold: threshold, resetAfter: resetAfter, isFailure: isFailure,
	}
}

// Do выполняет операцию, если цепь замкнута.
func (b *Breaker) Do(ctx context.Context, operation func(context.Context) error) error {
	if err := b.allow(); err != nil {
		return err
	}

	err := operation(ctx)
	b.record(err)
	return err
}

func (b *Breaker) allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == BreakerOpen {
		if time.Since(b.lastFailure) < b.resetAfter {
			return fmt.Errorf("%s: %w", b.name, ErrCircuitOpen)
		}
		// Пробный вызов: цепь не размыкается навсегда, иначе восстановление
		// зависимости останется незамеченным.
		b.state = BreakerHalfOpen
		slog.Info("Circuit half-open", slog.String("breaker", b.name))
	}
	return nil
}

func (b *Breaker) record(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err == nil || !b.isFailure(err) {
		if b.state != BreakerClosed {
			slog.Info("Circuit closed", slog.String("breaker", b.name))
		}
		b.state = BreakerClosed
		b.failures = 0
		return
	}

	b.failures++
	b.lastFailure = time.Now()
	// Из полуразомкнутого состояния одного отказа достаточно: зависимость
	// ещё не восстановилась, и очередь пробных вызовов ей не поможет.
	if b.state == BreakerHalfOpen || b.failures >= b.threshold {
		if b.state != BreakerOpen {
			slog.Warn("Circuit open",
				slog.String("breaker", b.name), slog.Int("failures", b.failures))
		}
		b.state = BreakerOpen
	}
}

// State возвращает текущее состояние.
func (b *Breaker) State() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}
