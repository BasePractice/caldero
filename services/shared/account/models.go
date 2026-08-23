package account

import (
	"time"

	"github.com/google/uuid"
)

type Account struct {
	Id        int64      `json:"id"`
	UserId    uuid.UUID  `json:"user_id"`
	Type      string     `json:"type"`
	CreditId  *uuid.UUID `json:"credit_id,omitempty"`
	State     string     `json:"state"`
	Balance   int64      `json:"balance"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

type CreateAccount struct {
	UserId   uuid.UUID  `json:"user_id"`
	Type     string     `json:"type"`
	CreditId *uuid.UUID `json:"credit_id"`
}

func (a CreateAccount) Validate() bool {
	if a.UserId == uuid.Nil {
		return false
	}
	// Согласовано с CHECK-ограничением схемы: кредитный счёт обязан ссылаться
	// на кредит, дебетовый — не может.
	switch a.Type {
	case "DEBIT":
		return a.CreditId == nil
	case "CREDIT":
		return a.CreditId != nil
	default:
		return false
	}
}

func (a CreateAccount) String() string {
	return "{user_id=" + a.UserId.String() + ", type=" + a.Type + "}"
}
