package credit

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// MaxMonth — верхняя граница срока кредита. Без неё срок задаёт размер
// среза платежей напрямую и приходит из запроса пользователя.
const MaxMonth = 600

type Percent uint

type Payment struct {
	CreditId  uint64    `json:"credit"`
	NeedValue uint      `json:"need_value"`
	Amount    uint      `json:"amount"`
	PaymentAt time.Time `json:"payment_at"`
	Status    string    `json:"status"`
}

type Credit struct {
	UserId      uuid.UUID
	CreatorId   uuid.UUID
	Type        string
	Kind        string
	Month       uint
	Percent     Percent
	Balance     uint
	AlreadyPaid uint
	CreatedAt   time.Time
	LastPaidAt  *time.Time
}

type CreateCredit struct {
	UserId      uuid.UUID  `json:"user_id"`
	Type        string     `json:"type" default:"SIMPLE"`
	Kind        string     `json:"kind" default:"ANN"`
	Month       uint       `json:"month" default:"6"`
	Percent     Percent    `json:"percent" default:"10"`
	Balance     uint       `json:"balance"`
	AlreadyPaid uint       `json:"already_payed"`
	CreatedAt   time.Time  `json:"created_at"`
	LastPaidAt  *time.Time `json:"last_payed_at"`
}

func (c CreateCredit) Validate() bool {
	return c.UserId != uuid.Nil && c.Percent >= 10 && c.Balance > 100 &&
		c.Month > 1 && c.Month <= MaxMonth
}

func (c CreateCredit) String() string {
	return fmt.Sprintf("{UserId: %s, Type: %s}", c.UserId, c.Type)
}
