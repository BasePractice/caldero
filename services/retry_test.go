package services

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetrySucceedsAfterFailures(t *testing.T) {
	attempts := 0
	policy := RetryPolicy{
		Attempts: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond,
		Description: "тест",
	}

	err := Retry(context.Background(), policy, func(context.Context) error {
		attempts++
		if attempts < 3 {
			return errors.New("временная ошибка")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if attempts != 3 {
		t.Errorf("попыток %d, ожидалось 3", attempts)
	}
}

func TestRetryStopsOnNonRetriable(t *testing.T) {
	permanent := errors.New("постоянная ошибка")
	attempts := 0
	policy := RetryPolicy{
		Attempts: 5, BaseDelay: time.Millisecond, Description: "тест",
		// Повторять ошибку, которая не пройдёт и со второй попытки, —
		// это просто задержка перед тем же отказом.
		Retriable: func(err error) bool { return !errors.Is(err, permanent) },
	}

	err := Retry(context.Background(), policy, func(context.Context) error {
		attempts++
		return permanent
	})
	if !errors.Is(err, permanent) {
		t.Fatalf("получено %v, ожидалась постоянная ошибка", err)
	}
	if attempts != 1 {
		t.Errorf("попыток %d, ожидалась 1", attempts)
	}
}

func TestRetryGivesUp(t *testing.T) {
	attempts := 0
	policy := RetryPolicy{Attempts: 3, BaseDelay: time.Millisecond, Description: "тест"}

	err := Retry(context.Background(), policy, func(context.Context) error {
		attempts++
		return errors.New("всегда падает")
	})
	if err == nil {
		t.Fatal("ожидалась ошибка после исчерпания попыток")
	}
	if attempts != 3 {
		t.Errorf("попыток %d, ожидалось 3", attempts)
	}
}

func TestRetryRespectsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0

	policy := RetryPolicy{Attempts: 10, BaseDelay: 50 * time.Millisecond, Description: "тест"}
	err := Retry(ctx, policy, func(context.Context) error {
		attempts++
		// Отмена во время ожидания между попытками должна прекращать
		// повторы немедленно, а не после исчерпания всех попыток.
		cancel()
		return errors.New("падает")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("получено %v, ожидалась context.Canceled", err)
	}
	if attempts != 1 {
		t.Errorf("попыток %d, ожидалась 1", attempts)
	}
}

func TestBackoffGrowsAndIsBounded(t *testing.T) {
	policy := RetryPolicy{BaseDelay: 100 * time.Millisecond, MaxDelay: 300 * time.Millisecond}

	previous := time.Duration(0)
	for attempt := range 5 {
		delay := backoff(policy, attempt)
		if delay > policy.MaxDelay {
			t.Fatalf("задержка %s превысила предел %s", delay, policy.MaxDelay)
		}
		if attempt < 2 && delay <= previous {
			t.Errorf("задержка не растёт: %s после %s", delay, previous)
		}
		previous = delay
	}
}

// TestDefaultRetryPolicy фиксирует значения по умолчанию: их меняют редко,
// а на поведение при недоступной зависимости они влияют напрямую.
func TestDefaultRetryPolicy(t *testing.T) {
	retriable := func(error) bool { return false }
	policy := DefaultRetryPolicy("вызов кошелька", retriable)

	if policy.Attempts != 3 {
		t.Errorf("попыток %d, ожидалось 3", policy.Attempts)
	}
	if policy.BaseDelay != 100*time.Millisecond || policy.MaxDelay != 2*time.Second {
		t.Errorf("задержки %s и %s", policy.BaseDelay, policy.MaxDelay)
	}
	if policy.Description != "вызов кошелька" {
		t.Errorf("описание %q потеряно", policy.Description)
	}
	if policy.Retriable == nil || policy.Retriable(errors.New("любая")) {
		t.Error("признак повторяемости не проброшен")
	}
}
