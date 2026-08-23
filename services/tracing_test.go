package services

import (
	"errors"
	"testing"

	"github.com/lib/pq"
)

// TestInitTracingDisabled: без адреса коллектора трассировка выключается,
// а не падает — большинство окружений работает без неё.
func TestInitTracingDisabled(t *testing.T) {
	stop, err := InitTracing(t.Context(), "test", Config{})
	if err != nil {
		t.Fatalf("выключенная трассировка вернула ошибку: %v", err)
	}
	if stop == nil {
		t.Fatal("функция остановки не возвращена")
	}
	stop(t.Context())
}

// TestInitTracingEnabled: экспортёр читает адрес из окружения сам,
// по спецификации OTLP, поэтому переменную ставит тест, а не конфигурация.
func TestInitTracingEnabled(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")

	stop, err := InitTracing(t.Context(), "test", Config{
		OTelEndpoint:    "http://127.0.0.1:4318",
		OTelSampleRatio: 0.5,
	})
	if err != nil {
		t.Fatalf("включение трассировки: %v", err)
	}
	// Коллектора нет, и остановка обязана завершиться по таймауту выгрузки,
	// а не повиснуть: иначе сервис не останавливается по сигналу.
	stop(t.Context())
}

func TestIsUniqueViolation(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"нарушение уникальности", &pq.Error{Code: "23505"}, true},
		{"обёрнутое нарушение", errors.Join(errors.New("создание счёта"), &pq.Error{Code: "23505"}), true},
		{"другое нарушение ограничения", &pq.Error{Code: "23514"}, false},
		{"обычная ошибка", errors.New("connection refused"), false},
		{"ошибки нет", nil, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsUniqueViolation(test.err); got != test.want {
				t.Errorf("получено %v, ожидалось %v", got, test.want)
			}
		})
	}
}
