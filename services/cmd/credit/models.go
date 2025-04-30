package main

import (
	"fmt"
	"github.com/google/uuid"
)

type Percent uint

type CreateCredit struct {
	UserId  uuid.UUID `json:"user_id"`
	Type    string    `json:"type" default:"SIMPLE"`
	Percent Percent   `json:"percent" default:"10"`
	Balance uint      `json:"balance"`
}

func (c CreateCredit) Validate() bool {
	return c.UserId != uuid.Nil && c.Percent >= 10 && c.Balance > 100
}

func (c CreateCredit) String() string {
	return fmt.Sprintf("{UserId: %s, Type; %s}", c.UserId, c.Type)
}
