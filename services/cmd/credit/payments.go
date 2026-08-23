package main

import (
	"fmt"
	"math"
	"time"

	"wish/services/shared/credit"
)

// MonthPayment FIXME: Сделать расчет оставшихся средств по кредиту и по процентам
type MonthPayment struct {
	CreditTail  uint      `json:"credit_tail"`
	PercentTail uint      `json:"percent_tail"`
	ExpiredAt   time.Time `json:"expired_at"`
	Value       uint      `json:"value"`
	Percent     uint      `json:"percent"`
}

func monthPaymentCalculation(c credit.Credit) ([]MonthPayment, error) {
	if c.Kind != "ANN" {
		return nil, fmt.Errorf("credit kind %q is not supported yet", c.Kind)
	}
	if c.Month == 0 || c.Month > credit.MaxMonth {
		return nil, fmt.Errorf("credit term must be between 1 and %d months, got %d",
			credit.MaxMonth, c.Month)
	}
	// При нулевой ставке формула аннуитета вырождается в 0/0.
	if c.Percent == 0 {
		return nil, fmt.Errorf("credit percent must be positive")
	}
	if c.AlreadyPaid > c.Balance {
		return nil, fmt.Errorf("paid amount %d exceeds credit balance %d",
			c.AlreadyPaid, c.Balance)
	}

	offset := time.Now()
	var paidMonth uint
	if c.LastPaidAt != nil {
		if c.LastPaidAt.Before(c.CreatedAt) {
			return nil, fmt.Errorf("last payment %s is earlier than credit creation %s",
				c.LastPaidAt.Format(time.DateOnly), c.CreatedAt.Format(time.DateOnly))
		}
		//FIXME: переделать, примерное значение
		paidMonth = uint(c.LastPaidAt.Sub(c.CreatedAt).Hours() / 24 / 30)
		offset = *c.LastPaidAt
	}
	// Месяцы беззнаковые: без этой проверки вычитание уходит в переполнение,
	// и make получает порядка 1.8e19 элементов.
	if paidMonth >= c.Month {
		return nil, fmt.Errorf("credit term of %d months is over: %d months already paid",
			c.Month, paidMonth)
	}

	needMonth := c.Month - paidMonth
	principal := float64(c.Balance - c.AlreadyPaid)
	monthPercent := float64(c.Percent) / 12 / 100
	growth := math.Pow(1+monthPercent, float64(needMonth))
	need := principal * monthPercent * growth / (growth - 1)

	payments := make([]MonthPayment, needMonth)
	for i := range payments {
		payments[i] = MonthPayment{
			ExpiredAt: offset,
			Value:     uint(math.Round(need)),
		}
		offset = offset.AddDate(0, 1, 0)
	}
	return payments, nil
}
