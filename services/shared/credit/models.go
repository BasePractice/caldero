package credit

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

const (
	// MaxMonth — верхняя граница срока кредита. Без неё срок задаёт размер
	// среза платежей напрямую и приходит из запроса пользователя.
	MaxMonth = 600

	// BasisPointsInPercent — базисных пунктов в одном проценте.
	// Ставка хранится в базисных пунктах, потому что целые проценты
	// не выражают даже 12,5 %, а дробные числа для денег не годятся.
	BasisPointsInPercent = 100
	// BasisPointsInWhole — базисных пунктов в единице (100 %).
	BasisPointsInWhole = 10_000

	// MinRate и MaxRate ограничивают ставку: 1 % и 300 % годовых.
	MinRate = 1 * BasisPointsInPercent
	MaxRate = 300 * BasisPointsInPercent
)

// Amount — денежная сумма в минимальных единицах (копейках).
// Дробные типы для денег не используются: 0.1 + 0.2 не равно 0.3,
// и ошибка накапливается на каждом платеже.
type Amount int64

func (a Amount) String() string {
	sign := ""
	if a < 0 {
		sign = "-"
		a = -a
	}
	return fmt.Sprintf("%s%d.%02d", sign, int64(a)/100, int64(a)%100)
}

// Rate — процентная ставка в базисных пунктах: 1250 это 12,5 %.
type Rate int32

func (r Rate) String() string {
	return fmt.Sprintf("%d.%02d%%", int32(r)/BasisPointsInPercent, int32(r)%BasisPointsInPercent)
}

type Payment struct {
	CreditId  uuid.UUID `json:"credit"`
	NeedValue Amount    `json:"need_value"`
	Amount    Amount    `json:"amount"`
	PaymentAt time.Time `json:"payment_at"`
	Status    string    `json:"status"`
}

type Credit struct {
	UserId      uuid.UUID
	CreatorId   uuid.UUID
	Type        string
	Kind        string
	Month       uint
	Rate        Rate
	Balance     Amount
	AlreadyPaid Amount
	CreatedAt   time.Time
	LastPaidAt  *time.Time
}

type CreateCredit struct {
	UserId      uuid.UUID  `json:"user_id"`
	Type        string     `json:"type"`
	Kind        string     `json:"kind"`
	Month       uint       `json:"month"`
	Rate        Rate       `json:"rate_bp"`
	Balance     Amount     `json:"balance"`
	AlreadyPaid Amount     `json:"already_paid"`
	CreatedAt   time.Time  `json:"created_at"`
	LastPaidAt  *time.Time `json:"last_paid_at"`
}

func (c CreateCredit) Validate() bool {
	return c.UserId != uuid.Nil &&
		c.Rate >= MinRate && c.Rate <= MaxRate &&
		c.Balance > 0 && c.AlreadyPaid >= 0 && c.AlreadyPaid < c.Balance &&
		c.Month > 1 && c.Month <= MaxMonth
}

func (c CreateCredit) String() string {
	return fmt.Sprintf("{UserId: %s, Type: %s, Balance: %s, Rate: %s}",
		c.UserId, c.Type, c.Balance, c.Rate)
}
