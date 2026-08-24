package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/smtp"
	"net/url"
	"strings"
	"time"

	"wish/services/shared/notify"

	"github.com/google/uuid"
)

// Contacts — то, что нужно знать о пользователе для доставки письмом.
// Интерфейс объявлен здесь, у потребителя.
type Contacts interface {
	// Contacts возвращает адрес и признак его подтверждения.
	Contacts(ctx context.Context, user uuid.UUID) (Contact, error)
}

// Contact — контакты пользователя из сервиса профиля.
type Contact struct {
	Email          string `json:"email"`
	EmailConfirmed bool   `json:"email_confirmed"`
}

// Sender письма — SMTP-релей. Своей очереди у него нет: повторы,
// ограничение частоты и учёт неудач уже есть в общем диспетчере,
// и вторая их реализация внутри канала разошлась бы с первой.
type Email struct {
	contacts Contacts
	config   EmailConfig
	// send подменяется в тестах: поднимать настоящий SMTP ради проверки
	// заголовков незачем.
	send func(addr string, auth smtp.Auth, from string, to []string, message []byte) error
}

// EmailConfig — настройки отправки.
type EmailConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	// From — адрес отправителя. Он должен быть тем, для которого настроены
	// SPF, DKIM и DMARC, иначе письма уходят в спам и канал бесполезен.
	From string
	// UnsubscribeBase — база ссылки отписки. Пустое значение отключает
	// канал: рассылка без отписки нарушает правила почтовых провайдеров
	// и ведёт прямиком в спам-листы.
	UnsubscribeBase string
	// Secret подписывает ссылку отписки.
	Secret string
}

func (c EmailConfig) Enabled() bool {
	return c.Host != "" && c.From != "" && c.UnsubscribeBase != "" && c.Secret != ""
}

// Validate объясняет, чего не хватает: канал, настроенный наполовину,
// молча не отправляет ничего.
func (c EmailConfig) Validate() error {
	missing := make([]string, 0, 4)
	for field, value := range map[string]string{
		"SMTP_HOST":             c.Host,
		"EMAIL_FROM":            c.From,
		"EMAIL_UNSUBSCRIBE_URL": c.UnsubscribeBase,
		"EMAIL_SECRET":          c.Secret,
	} {
		if value == "" {
			missing = append(missing, field)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("email channel is missing: %s", strings.Join(missing, ", "))
	}
	return nil
}

func NewEmail(contacts Contacts, config EmailConfig) *Email {
	return &Email{contacts: contacts, config: config, send: smtp.SendMail}
}

func (e *Email) Channel() notify.Channel { return notify.ChannelEmail }

func (e *Email) Send(ctx context.Context, task Task, title, body string) error {
	contact, err := e.contacts.Contacts(ctx, task.UserId)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrChannelUnavailable, err)
	}
	if contact.Email == "" {
		return ErrChannelUnbound
	}
	// На неподтверждённый адрес уходит только код подтверждения самого
	// адреса: иначе доставить его некуда. Всё остальное на непроверенный
	// адрес — это рассылка постороннему человеку.
	if !contact.EmailConfirmed && !confirmation(task.Type) {
		return fmt.Errorf("%w: address is not confirmed", ErrChannelUnbound)
	}

	message := e.compose(contact.Email, title, body, task.UserId)
	var auth smtp.Auth
	if e.config.Username != "" {
		auth = smtp.PlainAuth("", e.config.Username, e.config.Password, e.config.Host)
	}

	address := fmt.Sprintf("%s:%d", e.config.Host, e.config.Port)
	if err = e.send(address, auth, e.config.From, []string{contact.Email}, message); err != nil {
		// Отказ релея почти всегда временный: очередь и повторы уже есть
		// в диспетчере, и разбирать коды SMTP здесь незачем.
		return fmt.Errorf("%w: %w", ErrChannelUnavailable, err)
	}
	return nil
}

// confirmation сообщает, что событие само по себе подтверждает адрес.
func confirmation(eventType notify.EventType) bool {
	return eventType == notify.EventConfirmationCode || eventType == notify.EventConfirmationLink
}

// compose собирает письмо: заголовки, текстовую и HTML-часть.
//
// Обе части обязательны: почтовые клиенты, отключившие HTML, иначе
// показывают пустое письмо, а клиенты без текстовой части хуже ранжируются
// фильтрами.
func (e *Email) compose(to, title, body string, user uuid.UUID) []byte {
	unsubscribe := e.UnsubscribeLink(user)
	boundary := "wish-" + hex.EncodeToString([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))[:16]

	headers := []string{
		"From: " + e.config.From,
		"To: " + to,
		"Subject: " + mime.QEncoding.Encode("utf-8", title),
		"MIME-Version: 1.0",
		// Отписка в один клик: без этих заголовков письма считаются
		// рассылкой без согласия, и репутация отправителя падает.
		"List-Unsubscribe: <" + unsubscribe + ">",
		"List-Unsubscribe-Post: List-Unsubscribe=One-Click",
		`Content-Type: multipart/alternative; boundary="` + boundary + `"`,
	}

	message := &strings.Builder{}
	message.WriteString(strings.Join(headers, "\r\n"))
	message.WriteString("\r\n\r\n")

	message.WriteString("--" + boundary + "\r\n")
	message.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	message.WriteString(body + "\r\n\r\n")
	message.WriteString("Отписаться: " + unsubscribe + "\r\n\r\n")

	message.WriteString("--" + boundary + "\r\n")
	message.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
	fmt.Fprintf(message, "<html><body><h3>%s</h3><p>%s</p>"+
		`<p style="color:#777;font-size:12px">`+
		`<a href="%s">Отписаться от писем</a></p></body></html>`+"\r\n\r\n",
		escapeHTML(title), escapeHTML(body), unsubscribe)

	message.WriteString("--" + boundary + "--\r\n")
	return []byte(message.String())
}

// UnsubscribeLink собирает подписанную ссылку отписки.
//
// Подпись нужна, чтобы отписать чужого адресата было нельзя: без неё
// достаточно подставить в ссылку чужой идентификатор.
func (e *Email) UnsubscribeLink(user uuid.UUID) string {
	query := url.Values{}
	query.Set("user", user.String())
	query.Set("sign", e.sign(user))
	return strings.TrimRight(e.config.UnsubscribeBase, "/") + "?" + query.Encode()
}

// VerifyUnsubscribe проверяет подпись ссылки.
func (e *Email) VerifyUnsubscribe(user uuid.UUID, signature string) bool {
	expected, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return false
	}
	actual, err := base64.RawURLEncoding.DecodeString(e.sign(user))
	if err != nil {
		return false
	}
	// hmac.Equal, а не ==: сравнение с ранним выходом даёт возможность
	// подобрать подпись по времени ответа.
	return hmac.Equal(expected, actual)
}

func (e *Email) sign(user uuid.UUID) string {
	mac := hmac.New(sha256.New, []byte(e.config.Secret))
	// hash.Hash по контракту не возвращает ошибку записи.
	_, _ = mac.Write([]byte("unsubscribe:"))
	_, _ = mac.Write([]byte(user.String()))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// escapeHTML экранирует текст для HTML-части письма.
func escapeHTML(text string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return replacer.Replace(text)
}

// usersContacts читает контакты из сервиса пользователей.
type usersContacts struct {
	endpoint  string
	serviceId uuid.UUID
	client    *http.Client
}

func NewUsersContacts(endpoint string, serviceId uuid.UUID) Contacts {
	return &usersContacts{
		endpoint: endpoint, serviceId: serviceId,
		client: &http.Client{Timeout: 5 * time.Second},
	}
}

func (u *usersContacts) Contacts(ctx context.Context, user uuid.UUID) (Contact, error) {
	if u.endpoint == "" {
		return Contact{}, errors.New("users service is not configured")
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/users/%s/contacts", strings.TrimRight(u.endpoint, "/"), user), nil)
	if err != nil {
		return Contact{}, fmt.Errorf("creating contacts request: %w", err)
	}
	// Контакты — персональные данные, и отдаются только оператору.
	request.Header.Set("X-Authorized-Id", u.serviceId.String())
	request.Header.Set("X-Roles", "operator")

	response, err := u.client.Do(request)
	if err != nil {
		return Contact{}, fmt.Errorf("loading contacts of %s: %w", user, err)
	}
	defer func() {
		_ = response.Body.Close()
	}()

	if response.StatusCode != http.StatusOK {
		return Contact{}, fmt.Errorf("users answered %s", response.Status)
	}

	var contact Contact
	if err = json.NewDecoder(io.LimitReader(response.Body, 1<<16)).Decode(&contact); err != nil {
		return Contact{}, fmt.Errorf("decoding contacts: %w", err)
	}
	slog.DebugContext(ctx, "Contacts loaded", slog.String("user", user.String()))
	return contact, nil
}
