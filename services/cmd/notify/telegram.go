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
	"strings"
	"time"

	"wish/services/shared/notify"
)

// TelegramAPI — база адресов Bot API. Вынесена, чтобы тест мог подставить
// свой сервер: обращаться к настоящему Telegram из тестов нельзя.
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

// Telegram — доставка ботом и привязка аккаунта.
type Telegram struct {
	db     Database
	token  string
	api    string
	client *http.Client
}

func NewTelegram(db Database, token, api string) *Telegram {
	if api == "" {
		api = TelegramAPI
	}
	return &Telegram{
		db:    db,
		token: token,
		api:   api,
		// Таймаут запроса больше времени длинного опроса: иначе клиент
		// обрывал бы каждое ожидание обновлений.
		client: &http.Client{Timeout: telegramPollTimeout + telegramTimeout},
	}
}

func (t *Telegram) Channel() notify.Channel { return notify.ChannelTelegram }

// BindingCodeHash считает хеш кода привязки.
//
// В базе лежит хеш, а не код: список действующих кодов — это список
// готовых способов привязать чужой аккаунт к своему боту. Ключом служит
// токен бота: смена токена обесценивает старые коды, и это правильно —
// они всё равно живут минуты.
func (t *Telegram) BindingCodeHash(code string) []byte {
	mac := hmac.New(sha256.New, []byte(t.token))
	// hash.Hash по контракту не возвращает ошибку записи.
	_, _ = mac.Write([]byte(strings.ToUpper(code)))
	return mac.Sum(nil)
}

func (t *Telegram) Send(ctx context.Context, task Task, title, body string) error {
	binding, err := t.db.TelegramBinding(ctx, task.UserId)
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

	err = t.call(ctx, "sendMessage", map[string]any{
		"chat_id":                  binding.ChatId,
		"text":                     text,
		"disable_web_page_preview": true,
	}, nil)

	var api *telegramError
	if errors.As(err, &api) && api.Code == http.StatusForbidden {
		// Бот заблокирован пользователем. Отмечаем это, иначе каждое
		// следующее событие будет заново упираться в тот же отказ.
		if blockErr := t.db.BlockTelegram(ctx, task.UserId); blockErr != nil {
			slog.ErrorContext(ctx, "Can't mark telegram blocked",
				slog.String("user", task.UserId.String()), slog.String("err", blockErr.Error()))
		}
		return ErrChannelBlocked
	}
	return err
}

// Run обрабатывает обновления бота до отмены контекста: по команде
// /start с кодом привязывает чат к пользователю.
func (t *Telegram) Run(ctx context.Context) error {
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
			slog.WarnContext(ctx, "Can't read telegram updates", slog.String("err", err.Error()))
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

func (t *Telegram) handleCommand(ctx context.Context, chatId int64, text string) {
	fields := strings.Fields(text)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/start") {
		t.reply(ctx, chatId, "Чтобы получать оповещения, откройте привязку в приложении и пришлите команду /start с кодом.")
		return
	}
	if len(fields) < 2 {
		t.reply(ctx, chatId, "Нужен код привязки: /start КОД. Код выдаётся в настройках оповещений.")
		return
	}

	user, err := t.db.CompleteTelegramBinding(ctx, t.BindingCodeHash(fields[1]), chatId)
	if errors.Is(err, ErrNotFound) {
		// Причина не уточняется: по разнице ответов «код не найден»
		// и «код просрочен» подбирать код удобнее.
		t.reply(ctx, chatId, "Код не подошёл. Проверьте его в приложении или получите новый.")
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "Can't complete telegram binding", slog.String("err", err.Error()))
		t.reply(ctx, chatId, "Не получилось привязать аккаунт. Попробуйте позже.")
		return
	}

	slog.InfoContext(ctx, "Telegram bound", slog.String("user", user.String()))
	t.reply(ctx, chatId, "Аккаунт привязан. Оповещения будут приходить сюда.")
}

func (t *Telegram) reply(ctx context.Context, chatId int64, text string) {
	if err := t.call(ctx, "sendMessage", map[string]any{
		"chat_id": chatId,
		"text":    text,
	}, nil); err != nil {
		slog.WarnContext(ctx, "Can't reply in telegram", slog.String("err", err.Error()))
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

func (t *Telegram) updates(ctx context.Context, offset int64) ([]telegramUpdate, error) {
	var updates []telegramUpdate
	err := t.call(ctx, "getUpdates", map[string]any{
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

func (t *Telegram) call(ctx context.Context, method string, params map[string]any, result any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("encoding %s request: %w", method, err)
	}

	url := fmt.Sprintf("%s/bot%s/%s", t.api, t.token, method)
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
