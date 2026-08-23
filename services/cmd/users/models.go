package main

import (
	"database/sql"
	"time"

	"github.com/google/uuid"
)

type User struct {
	Id             uuid.UUID
	Username       string
	PasswordHash   string
	DisplayName    sql.NullString
	Email          sql.NullString
	Phone          sql.NullString
	PhoneConfirmed bool
	Gender         sql.NullString
	CreatedAt      time.Time
}

// Profile собирает полный профиль владельца.
func (u User) Profile() Profile {
	return Profile{
		Id:             u.Id,
		Username:       u.Username,
		DisplayName:    u.DisplayName.String,
		Email:          u.Email.String,
		Phone:          u.Phone.String,
		PhoneConfirmed: u.PhoneConfirmed,
		Gender:         u.Gender.String,
		AvatarURL:      AvatarURL(u.Email.String),
		CreatedAt:      u.CreatedAt,
	}
}

// PublicProfile собирает карточку, видимую посторонним.
func (u User) PublicProfile() PublicProfile {
	name := u.DisplayName.String
	if name == "" {
		// Имя входа как запасной вариант: показывать пустую карточку хуже,
		// чем показать логин, который пользователь и так предъявляет.
		name = u.Username
	}
	return PublicProfile{
		Id:          u.Id,
		DisplayName: name,
		AvatarURL:   AvatarURL(u.Email.String),
	}
}
