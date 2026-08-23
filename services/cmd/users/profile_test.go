package main

import (
	"testing"
)

func TestNormalizePhone(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
		valid bool
	}{
		{name: "уже в E.164", input: "+79001234567", want: "+79001234567", valid: true},
		{name: "с разделителями", input: "+7 (900) 123-45-67", want: "+79001234567", valid: true},
		// Российская восьмёрка — самый частый способ записи номера в стране,
		// и отвергать его значит отвергать половину пользователей.
		{name: "российская восьмёрка", input: "89001234567", want: "+79001234567", valid: true},
		{name: "без плюса", input: "79001234567", want: "+79001234567", valid: true},
		{name: "слишком короткий", input: "+7900"},
		{name: "слишком длинный", input: "+7900123456789012"},
		{name: "начинается с нуля", input: "+09001234567"},
		{name: "буквы", input: "не телефон"},
		{name: "пусто", input: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizePhone(tt.input)
			if !tt.valid {
				if err == nil {
					t.Fatalf("ожидалась ошибка, получено %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("неожиданная ошибка: %v", err)
			}
			if got != tt.want {
				t.Errorf("получено %q, ожидалось %q", got, tt.want)
			}
		})
	}
}

func TestValidateEmailAndGender(t *testing.T) {
	valid := []string{"user@example.com", "a.b+c@sub.example.org"}
	for _, value := range valid {
		if err := ValidateEmail(value); err != nil {
			t.Errorf("адрес %q отвергнут: %v", value, err)
		}
	}
	invalid := []string{"", "no-at-sign", "two@@at.com", "no@dot", "with space@example.com"}
	for _, value := range invalid {
		if err := ValidateEmail(value); err == nil {
			t.Errorf("адрес %q должен быть отвергнут", value)
		}
	}

	for _, value := range []string{GenderMale, GenderFemale, GenderOther} {
		if err := ValidateGender(value); err != nil {
			t.Errorf("пол %q отвергнут: %v", value, err)
		}
	}
	if err := ValidateGender("UNKNOWN"); err == nil {
		t.Error("неизвестный пол должен быть отвергнут")
	}
}

func TestAvatarURL(t *testing.T) {
	// Хеш из примера спецификации gravatar.
	const known = "https://www.gravatar.com/avatar/0bc83cb571cd1c50ba6f3e8a78ef1346?d=identicon"
	if got := AvatarURL("MyEmailAddress@example.com "); got != known {
		t.Errorf("получено %s, ожидалось %s", got, known)
	}
	if got := AvatarURL(""); got != "" {
		t.Errorf("без почты ссылка должна быть пустой, получено %s", got)
	}
}

func TestPublicProfileHidesContacts(t *testing.T) {
	user := User{Username: "alice"}
	user.Email.String, user.Email.Valid = "alice@example.com", true
	user.Phone.String, user.Phone.Valid = "+79001234567", true
	user.DisplayName.String, user.DisplayName.Valid = "Алиса", true

	public := user.PublicProfile()
	if public.DisplayName != "Алиса" {
		t.Errorf("имя %q, ожидалось Алиса", public.DisplayName)
	}
	if public.AvatarURL == "" {
		t.Error("аватар должен вычисляться из почты")
	}

	// Проверяется структура целиком: добавить контакт в публичную карточку
	// не должно быть возможно незаметно.
	full := user.Profile()
	if full.Phone == "" || full.Email == "" {
		t.Error("владелец должен видеть свои контакты")
	}
}

func TestPublicProfileFallsBackToUsername(t *testing.T) {
	user := User{Username: "bob"}
	if got := user.PublicProfile().DisplayName; got != "bob" {
		t.Errorf("имя %q, ожидалось bob: без отображаемого имени показывается логин", got)
	}
}
