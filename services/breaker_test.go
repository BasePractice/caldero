package services

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBreakerOpensAfterThreshold(t *testing.T) {
	failure := errors.New("зависимость недоступна")
	breaker := NewBreaker("тест", 3, time.Minute, nil)

	for range 3 {
		if err := breaker.Do(context.Background(), func(context.Context) error {
			return failure
		}); !errors.Is(err, failure) {
			t.Fatalf("получено %v, ожидалась исходная ошибка", err)
		}
	}
	if breaker.State() != BreakerOpen {
		t.Fatalf("состояние %s, ожидалось open", breaker.State())
	}

	// Разомкнутая цепь отклоняет вызов, не трогая зависимость: смысл
	// в том, чтобы перестать её добивать.
	called := false
	err := breaker.Do(context.Background(), func(context.Context) error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("получено %v, ожидалась ErrCircuitOpen", err)
	}
	if called {
		t.Error("операция не должна вызываться при разомкнутой цепи")
	}
}

func TestBreakerRecovers(t *testing.T) {
	failure := errors.New("падает")
	breaker := NewBreaker("тест", 2, 10*time.Millisecond, nil)

	for range 2 {
		_ = breaker.Do(context.Background(), func(context.Context) error { return failure })
	}
	if breaker.State() != BreakerOpen {
		t.Fatalf("состояние %s, ожидалось open", breaker.State())
	}

	time.Sleep(20 * time.Millisecond)
	// После паузы пропускается пробный вызов: цепь не размыкается навсегда,
	// иначе восстановление зависимости осталось бы незамеченным.
	if err := breaker.Do(context.Background(), func(context.Context) error {
		return nil
	}); err != nil {
		t.Fatalf("пробный вызов не прошёл: %v", err)
	}
	if breaker.State() != BreakerClosed {
		t.Errorf("состояние %s, ожидалось closed", breaker.State())
	}
}

func TestBreakerReopensOnHalfOpenFailure(t *testing.T) {
	failure := errors.New("падает")
	breaker := NewBreaker("тест", 1, 10*time.Millisecond, nil)

	_ = breaker.Do(context.Background(), func(context.Context) error { return failure })
	time.Sleep(20 * time.Millisecond)

	// Из полуразомкнутого состояния одного отказа достаточно: зависимость
	// ещё не восстановилась.
	_ = breaker.Do(context.Background(), func(context.Context) error { return failure })
	if breaker.State() != BreakerOpen {
		t.Errorf("состояние %s, ожидалось open", breaker.State())
	}
}

func TestBreakerIgnoresExpectedErrors(t *testing.T) {
	notFound := errors.New("не найдено")
	unavailable := errors.New("недоступно")
	breaker := NewBreaker("тест", 2, time.Minute, func(err error) bool {
		return errors.Is(err, unavailable)
	})

	// «Не найдено» — нормальный ответ, а не отказ зависимости.
	for range 5 {
		_ = breaker.Do(context.Background(), func(context.Context) error { return notFound })
	}
	if breaker.State() != BreakerClosed {
		t.Errorf("состояние %s, ожидалось closed", breaker.State())
	}
}

func TestBreakerStateString(t *testing.T) {
	tests := []struct {
		state BreakerState
		want  string
	}{
		{BreakerClosed, "closed"},
		{BreakerOpen, "open"},
		{BreakerHalfOpen, "half-open"},
		{BreakerState(42), "unknown"},
	}

	for _, test := range tests {
		t.Run(test.want, func(t *testing.T) {
			if got := test.state.String(); got != test.want {
				t.Errorf("получено %q, ожидалось %q", got, test.want)
			}
		})
	}
}

// TestNewBreakerDefaults: порог меньше единицы и отсутствующий признак
// отказа приходят из конфигурации, и размыкатель обязан оставаться
// работоспособным, а не делить на ноль.
func TestNewBreakerDefaults(t *testing.T) {
	breaker := NewBreaker("зависимость", 0, time.Minute, nil)
	if breaker.threshold != 1 {
		t.Errorf("порог %d, ожидался 1", breaker.threshold)
	}
	if breaker.isFailure == nil {
		t.Fatal("признак отказа не подставлен")
	}
	if !breaker.isFailure(errors.New("любая ошибка")) {
		t.Error("по умолчанию отказом должна считаться любая ошибка")
	}
}
