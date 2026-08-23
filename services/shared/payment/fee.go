package payment

import (
	"fmt"

	"wish/services/shared/credit"
)

// MaxAmount — верхняя граница суммы одной операции: 1 млрд рублей в копейках.
// Нужна не как продуктовое ограничение, а чтобы расчёт комиссии не мог
// переполнить int64: при этой границе и ставке до 100 % произведение
// остаётся на четыре порядка меньше предела типа.
const MaxAmount = credit.Amount(1_000_000_000_00)

// Fee — правило комиссии. Приходит из конфигурации, а не зашито в код:
// у каждого провайдера свои тарифы, и меняются они без изменения кода.
//
// Нулевое значение означает отсутствие комиссии и является рабочим.
type Fee struct {
	// BasisPoints — доля от суммы в базисных пунктах: 250 это 2,5 %.
	// Проценты дробным числом не хранятся по той же причине, что и деньги.
	BasisPoints int64
	// Fixed — фиксированная часть, добавляется к доле.
	Fixed credit.Amount
	// Min и Max ограничивают итог. Max, равный нулю, снимает верхнюю границу.
	Min credit.Amount
	Max credit.Amount
}

func (f Fee) Validate() error {
	if f.BasisPoints < 0 || f.BasisPoints > credit.BasisPointsInWhole {
		return fmt.Errorf("fee basis points must be between 0 and %d, got %d",
			credit.BasisPointsInWhole, f.BasisPoints)
	}
	if f.Fixed < 0 {
		return fmt.Errorf("fixed fee must not be negative, got %d", f.Fixed)
	}
	if f.Min < 0 {
		return fmt.Errorf("minimum fee must not be negative, got %d", f.Min)
	}
	if f.Max < 0 {
		return fmt.Errorf("maximum fee must not be negative, got %d", f.Max)
	}
	if f.Max > 0 && f.Max < f.Min {
		return fmt.Errorf("maximum fee %d is less than minimum %d", f.Max, f.Min)
	}
	return nil
}

// For считает комиссию с суммы. Сумма обязана быть в пределах MaxAmount —
// это проверяется валидацией запроса.
//
// Доля округляется вверх: при округлении вниз комиссия с мелких сумм
// обнуляется, и правило перестаёт действовать там, где оно нужнее всего.
func (f Fee) For(amount credit.Amount) credit.Amount {
	if amount <= 0 {
		return 0
	}
	if amount > MaxAmount {
		amount = MaxAmount
	}

	fee := f.Fixed
	if f.BasisPoints > 0 {
		product := int64(amount) * f.BasisPoints
		share := product / credit.BasisPointsInWhole
		if product%credit.BasisPointsInWhole != 0 {
			share++
		}
		fee += credit.Amount(share)
	}

	if fee < f.Min {
		fee = f.Min
	}
	if f.Max > 0 && fee > f.Max {
		fee = f.Max
	}
	return fee
}
