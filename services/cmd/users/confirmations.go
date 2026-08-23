package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ConfirmationKind — что подтверждается.
type ConfirmationKind string

const (
	ConfirmPhone ConfirmationKind = "PHONE"
	ConfirmEmail ConfirmationKind = "EMAIL"
)

func (k ConfirmationKind) Valid() bool {
	return k == ConfirmPhone || k == ConfirmEmail
}

// Значения по умолчанию для подтверждения.
const (
	// codeDigits — длина кода для телефона. Шесть цифр — это компромисс:
	// код диктуют по телефону и вводят руками, поэтому длину ограничивает
	// человек, а не стойкость. Стойкость обеспечивают срок жизни
	// и число попыток.
	codeDigits = 6
	// tokenBytes — длина токена для почты. Он попадает в ссылку, руками
	// его никто не вводит, и экономить на длине незачем.
	tokenBytes = 32
	// MaxAttempts — сколько раз можно ошибиться, прежде чем код сгорит.
	MaxAttempts = 5
)

// Ошибки подтверждения.
var (
	// ErrNoContact — контакт не заполнен в профиле.
	ErrNoContact = errors.New("contact is not set")
	// ErrAlreadyConfirmed — контакт уже подтверждён.
	ErrAlreadyConfirmed = errors.New("contact is already confirmed")
	// ErrTooOften — код запрашивают слишком часто.
	ErrTooOften = errors.New("confirmation is requested too often")
	// ErrNoConfirmation — действующего кода нет: не запрашивали,
	// он истёк или исчерпал попытки.
	ErrNoConfirmation = errors.New("no active confirmation")
	// ErrWrongCode — код не подошёл.
	ErrWrongCode = errors.New("confirmation code does not match")
	// ErrTargetChanged — контакт изменился с момента отправки кода.
	ErrTargetChanged = errors.New("contact has changed since the code was sent")
)

// Confirmation — выданный код подтверждения.
type Confirmation struct {
	Id       uuid.UUID        `json:"id"`
	UserId   uuid.UUID        `json:"user_id"`
	Kind     ConfirmationKind `json:"kind"`
	Target   string           `json:"target"`
	CodeHash []byte           `json:"-"`
	Attempts int              `json:"attempts"`
	// ExpiresAt отдаётся клиенту: он показывает, сколько ждать.
	ExpiresAt   time.Time  `json:"expires_at"`
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// Active сообщает, что код ещё можно предъявить.
func (c Confirmation) Active(now time.Time) bool {
	return c.ConfirmedAt == nil && c.Attempts < MaxAttempts && now.Before(c.ExpiresAt)
}

// NewCode выдаёт код подтверждения.
//
// crypto/rand: код открывает доступ к подтверждению контакта, а значит
// и к восстановлению доступа. Предсказуемая последовательность здесь
// равносильна отсутствию кода.
func NewCode(kind ConfirmationKind) (string, error) {
	if kind == ConfirmEmail {
		buffer := make([]byte, tokenBytes)
		if _, err := rand.Read(buffer); err != nil {
			return "", fmt.Errorf("generating confirmation token: %w", err)
		}
		return hex.EncodeToString(buffer), nil
	}

	var code strings.Builder
	for range codeDigits {
		digit, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", fmt.Errorf("generating confirmation code: %w", err)
		}
		code.WriteString(digit.String())
	}
	return code.String(), nil
}

// CodeHash считает хеш кода.
//
// Ключ — общий секрет сервиса: имея одну только базу, перебрать
// шестизначный код по хешу нельзя. Контакт и пользователь входят
// в хеш, поэтому код, выданный одному, не подойдёт другому.
func CodeHash(secret []byte, user uuid.UUID, kind ConfirmationKind, target, code string) []byte {
	mac := hmac.New(sha256.New, secret)
	// hash.Hash по контракту не возвращает ошибку записи.
	_, _ = mac.Write([]byte(user.String()))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(kind))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(strings.ToLower(target)))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(code))
	return mac.Sum(nil)
}

// MatchCode сверяет код с сохранённым хешем.
func MatchCode(secret []byte, confirmation Confirmation, code string) bool {
	// hmac.Equal, а не ==: сравнение с ранним выходом даёт возможность
	// подбирать хеш по времени ответа.
	return hmac.Equal(confirmation.CodeHash,
		CodeHash(secret, confirmation.UserId, confirmation.Kind, confirmation.Target, code))
}

// ConfirmationLink собирает ссылку подтверждения почты.
//
// Ссылка нужна потому, что тридцатидвухбайтный токен никто не станет
// переписывать руками. Пустой базовый адрес означает, что ссылку
// показать негде, и в письмо уйдёт один код.
func ConfirmationLink(base string, kind ConfirmationKind, code string) string {
	if base == "" {
		return ""
	}
	query := url.Values{}
	query.Set("kind", string(kind))
	query.Set("code", code)
	return strings.TrimRight(base, "/") + "/confirm?" + query.Encode()
}
