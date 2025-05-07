package account

import "github.com/google/uuid"

type Account struct {
}

type CreateAccount struct {
	UserId   uuid.UUID  `json:"user_id"`
	Type     string     `json:"type" default:"DEBIT"`
	CreditId *uuid.UUID `json:"credit_id"`
}

func (a CreateAccount) Validate() bool {
	return true
}

func (a CreateAccount) String() string {
	return "{user_id=" + a.UserId.String() + ", type=" + a.Type + "}"
}
