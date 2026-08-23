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
	EmailConfirmed bool
	Gender         sql.NullString
	// PasswordSet отличает «пароль не задан» от «задан»: у пользователя,
	// созданного через внешнего провайдера, пароля нет.
	PasswordSet bool
	CreatedAt   time.Time
}

// Complete сообщает, что профиль дозаполнен: телефон обязателен
// по требованию, и без него профиль неполон.
//
// Учётная запись при этом не блокируется: блокировка наказывала бы
// пользователя за то, что он пришёл через внешнего провайдера.
func (u User) Complete() bool {
	return u.Phone.Valid && u.Phone.String != ""
}

// Contact возвращает контакт нужного вида и признак его подтверждения.
//
// Собрано в одном месте: иначе каждая операция подтверждения повторяла бы
// выбор между двумя полями и рано или поздно перепутала бы их.
func (u User) Contact(kind ConfirmationKind) (string, bool) {
	if kind == ConfirmEmail {
		return u.Email.String, u.EmailConfirmed
	}
	return u.Phone.String, u.PhoneConfirmed
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
		EmailConfirmed: u.EmailConfirmed,
		Gender:         u.Gender.String,
		AvatarURL:      AvatarURL(u.Email.String),
		Complete:       u.Complete(),
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
