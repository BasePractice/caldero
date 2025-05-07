package account

import "github.com/google/uuid"

type Account struct {
}

type InputAccount struct {
	UserId   uuid.UUID  `json:"user_id"`
	Type     string     `json:"type" default:"DEBIT"`
	CreditId *uuid.UUID `json:"credit_id"`
}

func (a InputAccount) Validate() bool {
	return true
}

func (a InputAccount) String() string {
	return "{user_id=" + a.UserId.String() + ", type=" + a.Type + "}"
}
