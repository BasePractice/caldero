package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

var testSecret = []byte("test-confirmation-secret-32bytes")

func TestNewCode(t *testing.T) {
	t.Run("код для телефона состоит из цифр", func(t *testing.T) {
		digits := regexp.MustCompile(`^[0-9]{6}$`)
		for range 20 {
			code, err := NewCode(ConfirmPhone)
			if err != nil {
				t.Fatalf("генерация кода: %v", err)
			}
			// Код диктуют по телефону и вводят руками: буквы и знаки
			// здесь только мешают.
			if !digits.MatchString(code) {
				t.Fatalf("код %q не из шести цифр", code)
			}
		}
	})

	t.Run("токен для почты длинный", func(t *testing.T) {
		token, err := NewCode(ConfirmEmail)
		if err != nil {
			t.Fatalf("генерация токена: %v", err)
		}
		// Токен попадает в ссылку, руками его не вводят, и экономить
		// на длине незачем.
		if len(token) != tokenBytes*2 {
			t.Errorf("длина токена %d, ожидалось %d", len(token), tokenBytes*2)
		}
	})

	t.Run("коды не повторяются", func(t *testing.T) {
		seen := make(map[string]bool)
		for range 50 {
			code, err := NewCode(ConfirmPhone)
			if err != nil {
				t.Fatalf("генерация кода: %v", err)
			}
			seen[code] = true
		}
		// Полное совпадение всех пятидесяти означало бы, что источник
		// случайности не работает.
		if len(seen) < 40 {
			t.Errorf("из 50 кодов различны только %d", len(seen))
		}
	})
}

func TestCodeHash(t *testing.T) {
	user := uuid.New()
	other := uuid.New()
	hash := CodeHash(testSecret, user, ConfirmPhone, "+79000000000", "123456")

	if bytes.Equal(hash, CodeHash(testSecret, other, ConfirmPhone, "+79000000000", "123456")) {
		// Иначе код, выданный одному, подошёл бы другому.
		t.Error("хеш не зависит от пользователя")
	}
	if bytes.Equal(hash, CodeHash(testSecret, user, ConfirmEmail, "+79000000000", "123456")) {
		t.Error("хеш не зависит от вида подтверждения")
	}
	if bytes.Equal(hash, CodeHash(testSecret, user, ConfirmPhone, "+79000000001", "123456")) {
		t.Error("хеш не зависит от контакта")
	}
	if bytes.Equal(hash, CodeHash(testSecret, user, ConfirmPhone, "+79000000000", "123457")) {
		t.Error("хеш не зависит от кода")
	}
	if bytes.Equal(hash, CodeHash([]byte("другой секрет"), user, ConfirmPhone, "+79000000000", "123456")) {
		t.Error("хеш не зависит от секрета")
	}

	t.Run("регистр контакта не важен", func(t *testing.T) {
		lower := CodeHash(testSecret, user, ConfirmEmail, "user@example.com", "token")
		upper := CodeHash(testSecret, user, ConfirmEmail, "User@Example.COM", "token")
		if !bytes.Equal(lower, upper) {
			t.Error("почта в разном регистре дала разные хеши")
		}
	})
}

func TestMatchCode(t *testing.T) {
	user := uuid.New()
	confirmation := Confirmation{
		UserId: user, Kind: ConfirmPhone, Target: "+79000000000",
		CodeHash: CodeHash(testSecret, user, ConfirmPhone, "+79000000000", "123456"),
	}

	if !MatchCode(testSecret, confirmation, "123456") {
		t.Error("верный код не подошёл")
	}
	if MatchCode(testSecret, confirmation, "654321") {
		t.Error("неверный код подошёл")
	}
	if MatchCode([]byte("другой секрет"), confirmation, "123456") {
		t.Error("код подошёл с чужим секретом")
	}
}

func TestConfirmationActive(t *testing.T) {
	now := time.Now()
	confirmed := now.Add(-time.Minute)

	tests := []struct {
		name         string
		confirmation Confirmation
		want         bool
	}{
		{"свежий код", Confirmation{ExpiresAt: now.Add(time.Minute)}, true},
		{"истёкший код", Confirmation{ExpiresAt: now.Add(-time.Minute)}, false},
		{"исчерпаны попытки", Confirmation{
			ExpiresAt: now.Add(time.Minute), Attempts: MaxAttempts}, false},
		{"уже использован", Confirmation{
			ExpiresAt: now.Add(time.Minute), ConfirmedAt: &confirmed}, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.confirmation.Active(now); got != test.want {
				t.Errorf("Active() = %v, ожидалось %v", got, test.want)
			}
		})
	}
}

func TestConfirmationLink(t *testing.T) {
	link := ConfirmationLink("https://wish.example", ConfirmEmail, "abc123")
	if !strings.HasPrefix(link, "https://wish.example/confirm?") {
		t.Fatalf("ссылка: %q", link)
	}
	if !strings.Contains(link, "code=abc123") || !strings.Contains(link, "kind=EMAIL") {
		t.Errorf("в ссылке нет кода или вида: %q", link)
	}

	t.Run("лишняя косая черта не удваивается", func(t *testing.T) {
		if got := ConfirmationLink("https://wish.example/", ConfirmEmail, "abc"); strings.Contains(got, "//confirm") {
			t.Errorf("ссылка: %q", got)
		}
	})

	t.Run("без базового адреса ссылки нет", func(t *testing.T) {
		if got := ConfirmationLink("", ConfirmEmail, "abc"); got != "" {
			t.Errorf("ссылка без базового адреса: %q", got)
		}
	})
}

func TestConfirmationKindValid(t *testing.T) {
	for _, kind := range []ConfirmationKind{ConfirmPhone, ConfirmEmail} {
		if !kind.Valid() {
			t.Errorf("вид %s не признан известным", kind)
		}
	}
	if ConfirmationKind("TELEGRAM").Valid() {
		t.Error("неизвестный вид признан известным")
	}
}

func TestUserContact(t *testing.T) {
	user := User{}
	user.Phone.String, user.Phone.Valid = "+79000000000", true
	user.Email.String, user.Email.Valid = "user@example.com", true
	user.PhoneConfirmed = true

	if contact, confirmed := user.Contact(ConfirmPhone); contact != "+79000000000" || !confirmed {
		t.Errorf("телефон: %q, подтверждён: %v", contact, confirmed)
	}
	if contact, confirmed := user.Contact(ConfirmEmail); contact != "user@example.com" || confirmed {
		t.Errorf("почта: %q, подтверждена: %v", contact, confirmed)
	}
}
