package main

import (
	"testing"
	"time"

	"wish/services/shared/credit"
)

// Расчёт графика — единственная чистая функция с заметной арифметикой:
// на длинном сроке она считает сотни возведений в степень и выделяет срез
// на каждый месяц. Бенчмарк нужен, чтобы будущая оптимизация опиралась
// на измерение, а не на впечатление.
func BenchmarkMonthPaymentCalculation(b *testing.B) {
	cases := []struct {
		name  string
		month uint
	}{
		{name: "год", month: 12},
		{name: "пять лет", month: 60},
		{name: "тридцать лет", month: 360},
	}

	for _, tt := range cases {
		b.Run(tt.name, func(b *testing.B) {
			c := credit.Credit{
				Kind:      credit.KindAnnuity,
				Type:      credit.TypeSimple,
				Month:     tt.month,
				Rate:      24 * credit.BasisPointsInPercent,
				Balance:   120_000_000,
				CreatedAt: time.Date(2026, time.January, 15, 12, 0, 0, 0, time.UTC),
			}

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := monthPaymentCalculation(c); err != nil {
					b.Fatalf("расчёт: %v", err)
				}
			}
		})
	}
}
