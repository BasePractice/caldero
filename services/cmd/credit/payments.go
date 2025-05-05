package main

import (
	"log/slog"
	"math"
	"time"
)

// MonthPayment FIXME: Сделать расчет оставшихся средств по кредиту и по процентам
type MonthPayment struct {
	CreditTail  uint      `json:"credit_tail"`
	PercentTail uint      `json:"percent_tail"`
	ExpiredAt   time.Time `json:"expired_at"`
	Value       uint      `json:"value"`
	Percent     uint      `json:"percent"`
}

func mothPaymentCalculation(credit Credit) []MonthPayment {
	if credit.Kind == "ANN" {
		var summ = float64(credit.Balance - credit.AlreadyPayed)
		var alreadyPaidMonth uint
		var offset time.Time
		if credit.LastPayedAt == nil {
			alreadyPaidMonth = 0
			offset = time.Now()
		} else {
			//FIXME: переделать, примерное значение
			alreadyPaidMonth = uint(credit.LastPayedAt.Sub(credit.CreatedAt).Hours() / 24 / 30)
			offset = *credit.LastPayedAt
		}
		var needMonth = credit.Month - alreadyPaidMonth
		var monthPercent = float64(credit.Percent) / 12 / 100
		var need = (summ * monthPercent * math.Pow(1+monthPercent, float64(needMonth))) /
			(math.Pow(1+monthPercent, float64(needMonth)) - 1)
		payments := make([]MonthPayment, needMonth)

		for i := range payments {
			payments[i] = MonthPayment{
				ExpiredAt: offset,
				Value:     uint(need),
			}
			offset = offset.AddDate(0, 1, 0)
		}
		return payments
	} else {
		slog.Warn("credit kind not implement yet", slog.String("kind", credit.Kind))
		return []MonthPayment{}
	}
}
