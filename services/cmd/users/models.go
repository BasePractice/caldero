package main

import "github.com/google/uuid"

type User struct {
	Id           uuid.UUID
	Username     string
	PasswordHash string
}
