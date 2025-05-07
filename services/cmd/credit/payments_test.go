package main

import (
	"testing"
	"time"

	"wish/services/shared/credit"
)

func TestPaymentCalculation_Success(t *testing.T) {
	var c = credit.Credit{
		Kind:         "ANN",
		Type:         "SIMPLE",
		Month:        60,
		Percent:      10,
		Balance:      1_000_000,
		AlreadyPayed: 0,
		CreatedAt:    time.Now(),
		LastPayedAt:  nil,
	}

	t.Run("Расчет аннуитентного кредита на 5 лет", func(t *testing.T) {
		payments := mothPaymentCalculation(c)
		if len(payments) != 60 {
			t.Fatalf("Не правильное количество месяцев для платежа. Расчетное количество %d", len(payments))
		}
		if payments[0].Value != 21247 {
			t.Fatalf("Не правильно расчитан ежемесячный платеж. Расчетное значение %d", payments[0].Value)
		}
	})
	t.Run("Расчет аннуитентного кредита с оплатой", func(t *testing.T) {
		payments := mothPaymentCalculation(c)
		c.CreatedAt = time.Now().AddDate(0, -2, 0)
		c.AlreadyPayed = payments[0].Value
		c.LastPayedAt = &payments[0].ExpiredAt
		c.AlreadyPayed += payments[1].Value
		c.LastPayedAt = &payments[1].ExpiredAt
		payments = mothPaymentCalculation(c)
		if len(payments) != 57 {
			t.Fatalf("Не правильное количество месяцев для платежа. Расчетное количество %d", len(payments))
		}
		if payments[0].Value != 21171 {
			t.Fatalf("Не правильно расчитан ежемесячный платеж. Расчетное значение %d", payments[0].Value)
		}
	})
}
