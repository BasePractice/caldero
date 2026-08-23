package main

import (
	"fmt"
	"math"
	"time"

	"wish/services/shared/credit"
)

// MonthPayment FIXME: Сделать расчет оставшихся средств по кредиту и по процентам
type MonthPayment struct {
	CreditTail  credit.Amount `json:"credit_tail"`
	PercentTail credit.Amount `json:"percent_tail"`
	ExpiredAt   time.Time     `json:"expired_at"`
	Value       credit.Amount `json:"value"`
	Rate        credit.Rate   `json:"rate_bp"`
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
	if c.Rate <= 0 {
		return nil, fmt.Errorf("credit rate must be positive")
	}
	if c.AlreadyPaid >= c.Balance {
		return nil, fmt.Errorf("paid amount %s is not less than credit balance %s",
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
	// Ставка годовая и в базисных пунктах: 1250 -> 0.125 -> делим на 12.
	monthRate := float64(c.Rate) / credit.BasisPointsInWhole / 12
	growth := math.Pow(1+monthRate, float64(needMonth))
	// Округление, а не усечение: усечение систематически занижает платёж,
	// и на длинном сроке недобор становится заметным.
	need := credit.Amount(math.Round(principal * monthRate * growth / (growth - 1)))

	payments := make([]MonthPayment, needMonth)
	for i := range payments {
		payments[i] = MonthPayment{
			ExpiredAt: offset,
			Value:     need,
			Rate:      c.Rate,
		}
		offset = offset.AddDate(0, 1, 0)
	}
	return payments, nil
}
