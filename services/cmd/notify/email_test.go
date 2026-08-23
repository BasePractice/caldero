package main

import (
	"context"
	"errors"
	"net/smtp"
	"net/url"
	"strings"
	"testing"

	"wish/services/shared/notify"

	"github.com/google/uuid"
)

// fakeContacts подменяет сервис пользователей.
type fakeContacts struct {
	contact Contact
	err     error
}

func (f fakeContacts) Contacts(context.Context, uuid.UUID) (Contact, error) {
	return f.contact, f.err
}

// sentMail — то, что канал передал бы SMTP-релею.
type sentMail struct {
	from    string
	to      []string
	message string
	err     error
	calls   int
}

func newTestEmail(t *testing.T, contact Contact, mail *sentMail) *Email {
	t.Helper()

	email := NewEmail(fakeContacts{contact: contact}, EmailConfig{
		Host: "smtp.invalid", Port: 587, From: "Котелок <no-reply@wish.invalid>",
		UnsubscribeBase: "https://wish.invalid/notify/unsubscribe",
		Secret:          "unsubscribe-secret",
	})
	email.send = func(_ string, _ smtp.Auth, from string, to []string, message []byte) error {
		mail.calls++
		mail.from, mail.to, mail.message = from, to, string(message)
		return mail.err
	}
	return email
}

func TestEmailComposesLetter(t *testing.T) {
	mail := &sentMail{}
	email := newTestEmail(t, Contact{Email: "user@example.com", EmailConfirmed: true}, mail)

	task := testTask(0)
	task.Channel = notify.ChannelEmail
	if err := email.Send(context.Background(), task, "Подарок зарезервирован", "Кто-то дарит кофеварку"); err != nil {
		t.Fatalf("отправка письма: %v", err)
	}

	if mail.calls != 1 || len(mail.to) != 1 || mail.to[0] != "user@example.com" {
		t.Fatalf("письмо ушло не туда: %+v", mail.to)
	}

	// Отписка в один клик обязательна: без этих заголовков письма
	// считаются рассылкой без согласия.
	for _, header := range []string{"List-Unsubscribe:", "List-Unsubscribe-Post:"} {
		if !strings.Contains(mail.message, header) {
			t.Errorf("в письме нет заголовка %s", header)
		}
	}
	// Обе части обязательны: клиент без HTML иначе покажет пустое письмо,
	// а письмо без текстовой части хуже проходит фильтры.
	for _, part := range []string{"text/plain; charset=utf-8", "text/html; charset=utf-8"} {
		if !strings.Contains(mail.message, part) {
			t.Errorf("в письме нет части %s", part)
		}
	}
	if !strings.Contains(mail.message, "Subject: =?utf-8?") {
		t.Error("тема письма не закодирована: кириллица в заголовке требует кодирования")
	}
	if !strings.Contains(mail.message, "Отписаться:") {
		t.Error("в текстовой части нет ссылки отписки")
	}
}

func TestEmailRequiresConfirmedAddress(t *testing.T) {
	mail := &sentMail{}
	email := newTestEmail(t, Contact{Email: "user@example.com"}, mail)

	task := testTask(0)
	task.Channel = notify.ChannelEmail

	// Письмо на непроверенный адрес — это письмо постороннему человеку.
	if err := email.Send(context.Background(), task, "Заголовок", "Тело"); !errors.Is(err, ErrChannelUnbound) {
		t.Errorf("получено %v, ожидалась %v", err, ErrChannelUnbound)
	}
	if mail.calls != 0 {
		t.Error("письмо ушло на неподтверждённый адрес")
	}

	t.Run("код подтверждения — исключение", func(t *testing.T) {
		// Иначе подтвердить адрес было бы нечем.
		task.Type = notify.EventConfirmationCode
		if err := email.Send(context.Background(), task, "Код", "123456"); err != nil {
			t.Errorf("код подтверждения не ушёл: %v", err)
		}
		if mail.calls != 1 {
			t.Error("код подтверждения не отправлен")
		}
	})
}

func TestEmailWithoutAddress(t *testing.T) {
	mail := &sentMail{}
	email := newTestEmail(t, Contact{}, mail)

	task := testTask(0)
	task.Channel = notify.ChannelEmail
	if err := email.Send(context.Background(), task, "Заголовок", "Тело"); !errors.Is(err, ErrChannelUnbound) {
		t.Errorf("получено %v, ожидалась %v", err, ErrChannelUnbound)
	}
}

func TestEmailRelayFailure(t *testing.T) {
	mail := &sentMail{err: errors.New("релей недоступен")}
	email := newTestEmail(t, Contact{Email: "user@example.com", EmailConfirmed: true}, mail)

	task := testTask(0)
	task.Channel = notify.ChannelEmail
	// Отказ релея почти всегда временный: повторы уже есть в диспетчере.
	if err := email.Send(context.Background(), task, "Заголовок", "Тело"); !errors.Is(err, ErrChannelUnavailable) {
		t.Errorf("получено %v, ожидалась %v", err, ErrChannelUnavailable)
	}
}

func TestUnsubscribeSignature(t *testing.T) {
	email := newTestEmail(t, Contact{}, &sentMail{})
	user := uuid.New()

	link := email.UnsubscribeLink(user)
	if !strings.Contains(link, user.String()) || !strings.Contains(link, "sign=") {
		t.Fatalf("ссылка отписки: %q", link)
	}

	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatalf("разбор ссылки: %v", err)
	}
	signature := parsed.Query().Get("sign")
	if !email.VerifyUnsubscribe(user, signature) {
		t.Error("своя подпись не прошла проверку")
	}
	// Без подписи достаточно подставить чужой идентификатор, чтобы
	// отписать постороннего.
	if email.VerifyUnsubscribe(uuid.New(), signature) {
		t.Error("подпись подошла к чужому идентификатору")
	}
	if email.VerifyUnsubscribe(user, "не подпись") {
		t.Error("испорченная подпись прошла проверку")
	}
}

func TestEmailConfigValidation(t *testing.T) {
	full := EmailConfig{
		Host: "smtp.invalid", From: "no-reply@wish.invalid",
		UnsubscribeBase: "https://wish.invalid/unsubscribe", Secret: "secret",
	}
	if !full.Enabled() || full.Validate() != nil {
		t.Error("полная настройка отклонена")
	}

	t.Run("без отписки канал не включается", func(t *testing.T) {
		// Рассылка без отписки нарушает правила почтовых провайдеров
		// и ведёт в спам-листы.
		partial := full
		partial.UnsubscribeBase = ""
		if partial.Enabled() {
			t.Error("канал включён без ссылки отписки")
		}
		if err := partial.Validate(); err == nil ||
			!strings.Contains(err.Error(), "EMAIL_UNSUBSCRIBE_URL") {
			t.Errorf("в ошибке не сказано, чего не хватает: %v", err)
		}
	})
}

func TestLoadMessengers(t *testing.T) {
	db := &fakeDatabase{}

	messengers, err := LoadMessengers(db, "token", "https://api.invalid", "wish_bot")
	if err != nil {
		t.Fatalf("разбор мессенджеров: %v", err)
	}
	if len(messengers) != 1 || messengers[notify.ChannelTelegram] == nil {
		t.Fatalf("мессенджеров %d, ожидался один Telegram", len(messengers))
	}

	t.Run("без токена бот не поднимается", func(t *testing.T) {
		empty, err := LoadMessengers(db, "", "", "")
		if err != nil {
			t.Fatalf("разбор мессенджеров: %v", err)
		}
		if len(empty) != 0 {
			t.Errorf("мессенджеров %d, ожидалось ноль", len(empty))
		}
	})

	t.Run("неполная настройка площадки роняет старт", func(t *testing.T) {
		// Протокол чужой площадки задаётся конфигурацией, и половина
		// настроек означает канал, который молча ничего не отправит.
		t.Setenv("NOTIFY_MAX_TOKEN", "max-token")
		if _, err := LoadMessengers(db, "", "", ""); err == nil {
			t.Error("бот с неполной настройкой принят")
		}
	})

	t.Run("полная настройка площадки принимается", func(t *testing.T) {
		t.Setenv("NOTIFY_MAX_TOKEN", "max-token")
		t.Setenv("NOTIFY_MAX_API", "https://botapi.invalid")
		t.Setenv("NOTIFY_MAX_METHOD_PATH", "/{method}?access_token={token}")
		t.Setenv("NOTIFY_MAX_SEND_METHOD", "messages")
		t.Setenv("NOTIFY_MAX_UPDATES_METHOD", "updates")
		t.Setenv("NOTIFY_MAX_CHAT_FIELD", "chat_id")
		t.Setenv("NOTIFY_MAX_TEXT_FIELD", "text")

		loaded, err := LoadMessengers(db, "", "", "")
		if err != nil {
			t.Fatalf("разбор мессенджеров: %v", err)
		}
		if loaded[notify.ChannelMax] == nil {
			t.Error("МАКС не поднялся при полной настройке")
		}
	})
}
