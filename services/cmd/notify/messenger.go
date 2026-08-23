package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"wish/services/shared/notify"
)

// TelegramAPI — база адресов Bot API Telegram. Вынесена, чтобы тест мог
// подставить свой сервер: обращаться к настоящему Telegram из тестов нельзя.
const TelegramAPI = "https://api.telegram.org"

const (
	// telegramTimeout ограничивает обычный запрос к Bot API.
	telegramTimeout = 10 * time.Second
	// telegramPollTimeout — сколько Bot API держит запрос обновлений,
	// пока их нет. Длинный опрос дешевле частых коротких запросов.
	telegramPollTimeout = 25 * time.Second
	// maxMessageLength — предел Bot API на длину сообщения.
	maxMessageLength = 4096
)

// MessengerConfig описывает бота конкретного мессенджера.
//
// Протокол задаётся конфигурацией, а не кодом: у ботов один и тот же
// набор действий — отправить сообщение, забрать обновления, узнать
// о блокировке, — но адреса и имена полей у каждой площадки свои.
// Выдумывать чужой формат нельзя, поэтому он приходит извне, а значения
// по умолчанию заданы только для Telegram, где они известны.
type MessengerConfig struct {
	Channel notify.Channel
	// API — база адресов, Token — токен бота, BotName нужен для ссылки
	// привязки.
	API     string
	Token   string
	BotName string
	// MethodPath — шаблон пути метода. Подстановки: {token} и {method}.
	MethodPath string
	// SendMethod и UpdatesMethod — имена методов отправки и получения
	// обновлений.
	SendMethod    string
	UpdatesMethod string
	// ChatField и TextField — имена полей в запросе отправки.
	ChatField string
	TextField string
}

// TelegramConfig собирает настройки Telegram: у него формат известен,
// и повторять его в конфигурации стенда незачем.
func TelegramConfig(token, api, botName string) MessengerConfig {
	if api == "" {
		api = TelegramAPI
	}
	return MessengerConfig{
		Channel: notify.ChannelTelegram, API: api, Token: token, BotName: botName,
		MethodPath: "/bot{token}/{method}",
		SendMethod: "sendMessage", UpdatesMethod: "getUpdates",
		ChatField: "chat_id", TextField: "text",
	}
}

// Validate проверяет, что бота можно поднять: без адреса и токена
// он не отправит ничего, и узнать об этом лучше при старте.
func (c MessengerConfig) Validate() error {
	missing := make([]string, 0, 4)
	for field, value := range map[string]string{
		"API": c.API, "TOKEN": c.Token, "METHOD_PATH": c.MethodPath,
		"SEND_METHOD": c.SendMethod, "CHAT_FIELD": c.ChatField, "TEXT_FIELD": c.TextField,
	} {
		if value == "" {
			missing = append(missing, field)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("messenger %s is missing: %s", c.Channel, strings.Join(missing, ", "))
	}
	return nil
}

// Messenger — доставка ботом и привязка аккаунта.
type Messenger struct {
	db     Database
	config MessengerConfig
	client *http.Client
}

func NewMessenger(db Database, config MessengerConfig) *Messenger {
	return &Messenger{
		db:     db,
		config: config,
		// Таймаут запроса больше времени длинного опроса: иначе клиент
		// обрывал бы каждое ожидание обновлений.
		client: &http.Client{Timeout: telegramPollTimeout + telegramTimeout},
	}
}

func (t *Messenger) Channel() notify.Channel { return t.config.Channel }

// BindingCodeHash считает хеш кода привязки.
//
// В базе лежит хеш, а не код: список действующих кодов — это список
// готовых способов привязать чужой аккаунт к своему боту. Ключом служит
// токен бота: смена токена обесценивает старые коды, и это правильно —
// они всё равно живут минуты.
func (t *Messenger) BindingCodeHash(code string) []byte {
	mac := hmac.New(sha256.New, []byte(t.config.Token))
	// hash.Hash по контракту не возвращает ошибку записи.
	_, _ = mac.Write([]byte(strings.ToUpper(code)))
	return mac.Sum(nil)
}

func (t *Messenger) Send(ctx context.Context, task Task, title, body string) error {
	binding, err := t.db.MessengerBinding(ctx, t.config.Channel, task.UserId)
	if errors.Is(err, ErrNotFound) {
		return ErrChannelUnbound
	}
	if err != nil {
		return fmt.Errorf("loading telegram binding: %w", err)
	}
	if binding.Blocked {
		return ErrChannelBlocked
	}

	// Разметка не используется: в тексте оказываются названия товаров
	// и имена, введённые людьми, и любая разметка на них ломается.
	text := title + "\n\n" + body
	if len(text) > maxMessageLength {
		text = text[:maxMessageLength]
	}

	err = t.call(ctx, t.config.SendMethod, map[string]any{
		t.config.ChatField: binding.ChatId,
		t.config.TextField: text,
	}, nil)

	var api *telegramError
	if errors.As(err, &api) && api.Code == http.StatusForbidden {
		// Бот заблокирован пользователем. Отмечаем это, иначе каждое
		// следующее событие будет заново упираться в тот же отказ.
		if blockErr := t.db.BlockMessenger(ctx, t.config.Channel, task.UserId); blockErr != nil {
			slog.ErrorContext(ctx, "Can't mark messenger blocked",
				slog.String("user", task.UserId.String()), slog.String("err", blockErr.Error()))
		}
		return ErrChannelBlocked
	}
	return err
}

// Run обрабатывает обновления бота до отмены контекста: по команде
// /start с кодом привязывает чат к пользователю.
func (t *Messenger) Run(ctx context.Context) error {
	var offset int64
	for {
		if ctx.Err() != nil {
			return nil
		}

		updates, err := t.updates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.WarnContext(ctx, "Can't read messenger updates",
				slog.String("channel", string(t.config.Channel)), slog.String("err", err.Error()))
			// Пауза после сбоя: без неё цикл превращается в непрерывный
			// поток запросов к недоступному API.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(telegramTimeout):
			}
			continue
		}

		for _, update := range updates {
			// Смещение подтверждает обработку: Bot API повторяет
			// неподтверждённые обновления бесконечно.
			offset = update.UpdateId + 1
			if update.Message == nil {
				continue
			}
			t.handleCommand(ctx, update.Message.Chat.Id, update.Message.Text)
		}
	}
}

func (t *Messenger) handleCommand(ctx context.Context, chatId int64, text string) {
	fields := strings.Fields(text)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/start") {
		t.reply(ctx, chatId, "Чтобы получать оповещения, откройте привязку в приложении и пришлите команду /start с кодом.")
		return
	}
	if len(fields) < 2 {
		t.reply(ctx, chatId, "Нужен код привязки: /start КОД. Код выдаётся в настройках оповещений.")
		return
	}

	user, err := t.db.CompleteMessengerBinding(ctx, t.config.Channel, t.BindingCodeHash(fields[1]), chatId)
	if errors.Is(err, ErrNotFound) {
		// Причина не уточняется: по разнице ответов «код не найден»
		// и «код просрочен» подбирать код удобнее.
		t.reply(ctx, chatId, "Код не подошёл. Проверьте его в приложении или получите новый.")
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "Can't complete messenger binding",
			slog.String("channel", string(t.config.Channel)), slog.String("err", err.Error()))
		t.reply(ctx, chatId, "Не получилось привязать аккаунт. Попробуйте позже.")
		return
	}

	slog.InfoContext(ctx, "Messenger bound",
		slog.String("channel", string(t.config.Channel)), slog.String("user", user.String()))
	t.reply(ctx, chatId, "Аккаунт привязан. Оповещения будут приходить сюда.")
}

func (t *Messenger) reply(ctx context.Context, chatId int64, text string) {
	if err := t.call(ctx, t.config.SendMethod, map[string]any{
		t.config.ChatField: chatId,
		t.config.TextField: text,
	}, nil); err != nil {
		slog.WarnContext(ctx, "Can't reply in messenger", slog.String("err", err.Error()))
	}
}

type telegramUpdate struct {
	UpdateId int64 `json:"update_id"`
	Message  *struct {
		Chat struct {
			Id int64 `json:"id"`
		} `json:"chat"`
		Text string `json:"text"`
	} `json:"message"`
}

func (t *Messenger) updates(ctx context.Context, offset int64) ([]telegramUpdate, error) {
	var updates []telegramUpdate
	err := t.call(ctx, t.config.UpdatesMethod, map[string]any{
		"offset":          offset,
		"timeout":         int(telegramPollTimeout.Seconds()),
		"allowed_updates": []string{"message"},
	}, &updates)
	return updates, err
}

// telegramError — отказ, о котором сообщил сам Bot API.
type telegramError struct {
	Code        int
	Description string
}

func (e *telegramError) Error() string {
	return fmt.Sprintf("telegram api error %d: %s", e.Code, e.Description)
}

func (t *Messenger) call(ctx context.Context, method string, params map[string]any, result any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("encoding %s request: %w", method, err)
	}

	path := strings.ReplaceAll(t.config.MethodPath, "{token}", t.config.Token)
	path = strings.ReplaceAll(path, "{method}", method)
	url := t.config.API + path
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating %s request: %w", method, err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := t.client.Do(request)
	if err != nil {
		return fmt.Errorf("%w: calling %s: %s", ErrChannelUnavailable, method, err)
	}
	defer func() {
		// Тело читается до конца выше; здесь остаётся только закрыть.
		_ = response.Body.Close()
	}()

	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%w: reading %s response: %s", ErrChannelUnavailable, method, err)
	}

	var envelope struct {
		Ok          bool            `json:"ok"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err = json.Unmarshal(payload, &envelope); err != nil {
		return fmt.Errorf("%w: decoding %s response: %s", ErrChannelUnavailable, method, err)
	}
	if !envelope.Ok {
		apiErr := &telegramError{Code: envelope.ErrorCode, Description: envelope.Description}
		if apiErr.Code == 0 {
			apiErr.Code = response.StatusCode
		}
		switch {
		case apiErr.Code == http.StatusForbidden:
			return apiErr
		case apiErr.Code == http.StatusBadRequest:
			// Чат не найден или удалён: повторять нечего.
			return fmt.Errorf("%w: %s", ErrChannelUnbound, apiErr.Description)
		default:
			// Ограничение частоты и сбои самого Bot API имеет смысл
			// повторить позже.
			return fmt.Errorf("%w: %s", ErrChannelUnavailable, apiErr)
		}
	}

	if result != nil {
		if err = json.Unmarshal(envelope.Result, result); err != nil {
			return fmt.Errorf("decoding %s result: %w", method, err)
		}
	}
	return nil
}

// NewBindingCode выдаёт код привязки. Алфавит без похожих знаков: код
// переписывают руками, и различить 0 и O в мессенджере невозможно.
func NewBindingCode(random func([]byte) (int, error)) (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	const length = 8

	buffer := make([]byte, length)
	if _, err := random(buffer); err != nil {
		return "", fmt.Errorf("generating binding code: %w", err)
	}
	code := make([]byte, length)
	for i, b := range buffer {
		code[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(code), nil
}

// BindingLink собирает ссылку, по которой бот получает код без ручного ввода.
func BindingLink(botName, code string) string {
	if botName == "" {
		return ""
	}
	return fmt.Sprintf("https://t.me/%s?start=%s", botName, code)
}

// LoadMessengers читает настройки ботов из окружения.
//
// Разбор живёт в сервисе, а не в общей конфигурации: набор переменных
// зависит от числа мессенджеров и нужен только здесь. Telegram описан
// значениями по умолчанию — его формат известен; для остальных площадок
// формат задаётся явно, потому что выдумывать чужой протокол нельзя.
func LoadMessengers(db Database, telegramToken, telegramAPI, telegramBot string) (map[notify.Channel]*Messenger, error) {
	messengers := make(map[notify.Channel]*Messenger, 2)

	if telegramToken != "" {
		config := TelegramConfig(telegramToken, telegramAPI, telegramBot)
		if err := config.Validate(); err != nil {
			return nil, err
		}
		messengers[notify.ChannelTelegram] = NewMessenger(db, config)
	}

	if token := os.Getenv("NOTIFY_MAX_TOKEN"); token != "" {
		config := MessengerConfig{
			Channel:       notify.ChannelMax,
			API:           os.Getenv("NOTIFY_MAX_API"),
			Token:         token,
			BotName:       os.Getenv("NOTIFY_MAX_BOT"),
			MethodPath:    os.Getenv("NOTIFY_MAX_METHOD_PATH"),
			SendMethod:    os.Getenv("NOTIFY_MAX_SEND_METHOD"),
			UpdatesMethod: os.Getenv("NOTIFY_MAX_UPDATES_METHOD"),
			ChatField:     os.Getenv("NOTIFY_MAX_CHAT_FIELD"),
			TextField:     os.Getenv("NOTIFY_MAX_TEXT_FIELD"),
		}
		if err := config.Validate(); err != nil {
			return nil, err
		}
		messengers[notify.ChannelMax] = NewMessenger(db, config)
	}
	return messengers, nil
}
