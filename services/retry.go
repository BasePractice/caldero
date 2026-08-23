package services

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"time"
)

// RetryPolicy описывает повторы. Повторять можно только идемпотентные
// операции: для денежных операций повтор без ключа идемпотентности
// проведёт списание дважды.
type RetryPolicy struct {
	Attempts    int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Retriable   func(error) bool
	Description string
}

// DefaultRetryPolicy — разумные значения для сетевых вызовов.
func DefaultRetryPolicy(description string, retriable func(error) bool) RetryPolicy {
	return RetryPolicy{
		Attempts:    3,
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    2 * time.Second,
		Retriable:   retriable,
		Description: description,
	}
}

// Retry выполняет операцию с повторами. Задержка растёт экспоненциально
// с джиттером: одинаковая задержка у всех клиентов приводит к тому, что
// они возвращаются к упавшей зависимости одновременно и роняют её снова.
func Retry(ctx context.Context, policy RetryPolicy, operation func(context.Context) error) error {
	if policy.Attempts < 1 {
		policy.Attempts = 1
	}

	var lastErr error
	for attempt := range policy.Attempts {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%s: %w", policy.Description, err)
		}

		lastErr = operation(ctx)
		if lastErr == nil {
			return nil
		}
		if policy.Retriable != nil && !policy.Retriable(lastErr) {
			return lastErr
		}
		if attempt == policy.Attempts-1 {
			break
		}

		delay := backoff(policy, attempt)
		slog.DebugContext(ctx, "Retrying after failure",
			slog.String("operation", policy.Description),
			slog.Int("attempt", attempt+1),
			slog.Duration("delay", delay),
			slog.String("err", lastErr.Error()))

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("%s: %w", policy.Description, ctx.Err())
		case <-timer.C:
		}
	}
	return fmt.Errorf("%s: giving up after %d attempts: %w",
		policy.Description, policy.Attempts, lastErr)
}

func backoff(policy RetryPolicy, attempt int) time.Duration {
	delay := time.Duration(float64(policy.BaseDelay) * math.Pow(2, float64(attempt)))
	if policy.MaxDelay > 0 && delay > policy.MaxDelay {
		delay = policy.MaxDelay
	}
	// Джиттер в половину интервала: crypto/rand, потому что math/rand
	// в этом проекте не используется вовсе — так проще не перепутать его
	// с розыгрышем призов, где предсказуемость недопустима.
	half := int64(delay / 2)
	if half <= 0 {
		return delay
	}
	jitter, err := rand.Int(rand.Reader, big.NewInt(half))
	if err != nil {
		return delay
	}
	return delay/2 + time.Duration(jitter.Int64())
}
