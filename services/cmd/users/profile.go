package main

import (
	// MD5 здесь не средство защиты: этого алгоритма требует протокол
	// gravatar, и заменить его нельзя, не сломав совместимость.
	// Правило G501 отключено для этого файла в .golangci.yml.
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Пол пользователя. Значения совпадают с CHECK-ограничением схемы.
const (
	GenderMale   = "MALE"
	GenderFemale = "FEMALE"
	GenderOther  = "OTHER"
)

// phonePattern — формат E.164. Проверка формальная: она не подтверждает,
// что номер существует, поэтому обязательность телефона имеет смысл только
// вместе с подтверждением (поле phone_confirmed).
var phonePattern = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)

// emailPattern — минимальная проверка формы. Полная проверка почты
// регулярным выражением невозможна, единственный надёжный способ —
// отправить письмо.
var emailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s.]+\.[^@\s]+$`)

// Profile — полный профиль, доступный владельцу.
type Profile struct {
	Id             uuid.UUID `json:"id"`
	Username       string    `json:"username"`
	DisplayName    string    `json:"display_name,omitempty"`
	Email          string    `json:"email,omitempty"`
	Phone          string    `json:"phone,omitempty"`
	PhoneConfirmed bool      `json:"phone_confirmed"`
	Gender         string    `json:"gender,omitempty"`
	AvatarURL      string    `json:"avatar_url,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// PublicProfile — то, что видно посторонним по идентификатору.
//
// Контакты сюда не входят намеренно: идентификатор пользователя known
// каждому, кто с ним взаимодействовал, и отдавать по нему телефон
// и почту означает раздать контакты всей системе.
type PublicProfile struct {
	Id          uuid.UUID `json:"id"`
	DisplayName string    `json:"display_name"`
	AvatarURL   string    `json:"avatar_url,omitempty"`
}

// ProfileUpdate — изменяемые поля. Указатели отличают «не менять»
// от «очистить».
type ProfileUpdate struct {
	DisplayName *string `json:"display_name"`
	Email       *string `json:"email"`
	Phone       *string `json:"phone"`
	Gender      *string `json:"gender"`
}

// NormalizePhone приводит номер к E.164.
//
// Разбор упрощённый: убираются пробелы и разделители, российская восьмёрка
// заменяется на +7. Полноценный разбор требует базы кодов стран —
// если понадобится, его место здесь.
func NormalizePhone(value string) (string, error) {
	cleaned := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' || r == '+' {
			return r
		}
		return -1
	}, value)

	if strings.HasPrefix(cleaned, "8") && len(cleaned) == 11 {
		cleaned = "+7" + cleaned[1:]
	}
	if !strings.HasPrefix(cleaned, "+") {
		cleaned = "+" + cleaned
	}
	if !phonePattern.MatchString(cleaned) {
		return "", fmt.Errorf("phone must be in E.164 format, got %q", value)
	}
	return cleaned, nil
}

// ValidateEmail проверяет форму адреса.
func ValidateEmail(value string) error {
	if !emailPattern.MatchString(value) {
		return fmt.Errorf("email is malformed")
	}
	return nil
}

// ValidateGender проверяет значение пола.
func ValidateGender(value string) error {
	switch value {
	case GenderMale, GenderFemale, GenderOther:
		return nil
	default:
		return fmt.Errorf("gender must be one of %s, %s, %s", GenderMale, GenderFemale, GenderOther)
	}
}

// AvatarURL строит ссылку на gravatar. Аватар не хранится: он вычисляется
// из почты, и хранить его значило бы держать копию, которая устареет.
func AvatarURL(email string) string {
	if email == "" {
		return ""
	}
	sum := md5.Sum([]byte(strings.ToLower(strings.TrimSpace(email)))) //nolint:gosec // G401: см. комментарий у импорта
	return "https://www.gravatar.com/avatar/" + hex.EncodeToString(sum[:]) + "?d=identicon"
}
