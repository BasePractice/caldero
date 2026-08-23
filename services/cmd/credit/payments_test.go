package main

import (
	"testing"
	"time"

	"wish/services/shared/credit"
)

// created — фиксированная дата вместо time.Now(): число дней между
// «сейчас минус два месяца» и «сейчас плюс месяц» зависит от календаря,
// и такой тест ложно падал бы в отдельные месяцы.
var created = time.Date(2025, time.January, 15, 12, 0, 0, 0, time.UTC)

func at(days int) *time.Time {
	t := created.AddDate(0, 0, days)
	return &t
}

func TestMonthPaymentCalculation(t *testing.T) {
	base := credit.Credit{
		Kind:      "ANN",
		Type:      "SIMPLE",
		Month:     60,
		Percent:   10,
		Balance:   1_000_000,
		CreatedAt: created,
	}

	tests := []struct {
		name      string
		mutate    func(c *credit.Credit)
		wantCount int
		// Аннуитет: P * i * (1+i)^n / ((1+i)^n - 1), где i — месячная ставка.
		wantFirst uint
	}{
		{
			// 1 000 000 под 10 % на 60 месяцев: 21247.0447 -> 21247
			name:      "новый кредит на 5 лет",
			mutate:    func(*credit.Credit) {},
			wantCount: 60,
			wantFirst: 21247,
		},
		{
			// Погашено два месяца, остаток 957 506 на 58 месяцев: 20885.848 -> 20886.
			// Округление, а не усечение: усечение систематически занижает платёж.
			name: "кредит с двумя внесёнными платежами",
			mutate: func(c *credit.Credit) {
				c.AlreadyPaid = 42_494
				c.LastPaidAt = at(60)
			},
			wantCount: 58,
			wantFirst: 20886,
		},
		{
			name: "срок в один месяц",
			mutate: func(c *credit.Credit) {
				c.Month = 1
			},
			wantCount: 1,
			wantFirst: 1_008_333,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base
			tt.mutate(&c)

			payments, err := monthPaymentCalculation(c)
			if err != nil {
				t.Fatalf("неожиданная ошибка: %v", err)
			}
			if len(payments) != tt.wantCount {
				t.Fatalf("количество платежей = %d, ожидалось %d", len(payments), tt.wantCount)
			}
			if payments[0].Value != tt.wantFirst {
				t.Fatalf("первый платёж = %d, ожидалось %d", payments[0].Value, tt.wantFirst)
			}
		})
	}
}

func TestMonthPaymentCalculationRejects(t *testing.T) {
	base := credit.Credit{
		Kind:      "ANN",
		Type:      "SIMPLE",
		Month:     60,
		Percent:   10,
		Balance:   1_000_000,
		CreatedAt: created,
	}

	tests := []struct {
		name   string
		mutate func(c *credit.Credit)
	}{
		{
			name:   "неподдерживаемый вид кредита",
			mutate: func(c *credit.Credit) { c.Kind = "DYN" },
		},
		{
			name:   "нулевой срок",
			mutate: func(c *credit.Credit) { c.Month = 0 },
		},
		{
			name:   "срок за верхней границей",
			mutate: func(c *credit.Credit) { c.Month = credit.MaxMonth + 1 },
		},
		{
			// 0/0 в формуле аннуитета
			name:   "нулевая ставка",
			mutate: func(c *credit.Credit) { c.Percent = 0 },
		},
		{
			name: "внесено больше тела кредита",
			mutate: func(c *credit.Credit) {
				c.AlreadyPaid = c.Balance + 1
			},
		},
		{
			// Раньше здесь переполнялось беззнаковое вычитание, и make
			// пытался выделить порядка 1.8e19 элементов.
			name: "погашенных месяцев больше срока кредита",
			mutate: func(c *credit.Credit) {
				c.Month = 12
				c.LastPaidAt = at(400)
			},
		},
		{
			name: "последний платёж раньше выдачи кредита",
			mutate: func(c *credit.Credit) {
				c.LastPaidAt = at(-1)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base
			tt.mutate(&c)

			payments, err := monthPaymentCalculation(c)
			if err == nil {
				t.Fatalf("ожидалась ошибка, получено %d платежей", len(payments))
			}
			if payments != nil {
				t.Fatalf("при ошибке платежи должны быть nil, получено %d", len(payments))
			}
		})
	}
}
