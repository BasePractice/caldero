package account

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"wish/services/shared/credit"
)

type Account struct {
	Id       uuid.UUID  `json:"id"`
	UserId   uuid.UUID  `json:"user_id"`
	Type     string     `json:"type"`
	CreditId *uuid.UUID `json:"credit_id,omitempty"`
	State    string     `json:"state"`
	// Баланс в минимальных единицах, как и суммы кредита.
	Balance   credit.Amount `json:"balance"`
	StartedAt *time.Time    `json:"started_at,omitempty"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
}

type CreateAccount struct {
	UserId   uuid.UUID  `json:"user_id"`
	Type     string     `json:"type"`
	CreditId *uuid.UUID `json:"credit_id"`
}

// Типы счёта. Совпадают с CHECK-ограничением схемы.
const (
	TypeDebit  = "DEBIT"
	TypeCredit = "CREDIT"
)

// Validate возвращает причину отказа, а не просто false.
func (a CreateAccount) Validate() error {
	if a.UserId == uuid.Nil {
		return fmt.Errorf("user_id is required")
	}
	// Согласовано с CHECK-ограничением схемы: кредитный счёт обязан ссылаться
	// на кредит, дебетовый — не может.
	switch a.Type {
	case TypeDebit:
		if a.CreditId != nil {
			return fmt.Errorf("credit_id must be empty for a %s account", TypeDebit)
		}
		return nil
	case TypeCredit:
		if a.CreditId == nil {
			return fmt.Errorf("credit_id is required for a %s account", TypeCredit)
		}
		return nil
	default:
		return fmt.Errorf("type must be one of %s, %s", TypeDebit, TypeCredit)
	}
}

func (a CreateAccount) String() string {
	return "{user_id=" + a.UserId.String() + ", type=" + a.Type + "}"
}
