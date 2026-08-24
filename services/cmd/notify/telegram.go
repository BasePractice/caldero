package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"wish/services/shared/notify"
)

// TelegramAPI — база адресов Bot API Telegram. Вынесена, чтобы тест мог
// подставить свой сервер: обращаться к настоящему Telegram из тестов нельзя.
const TelegramAPI = "https://api.telegram.org"

// telegramMessageLimit — предел Bot API на длину сообщения.
const telegramMessageLimit = 4096

// NewTelegram собирает бота Telegram.
func NewTelegram(db Database, token, api, botName string) *Messenger {
	if api == "" {
		api = TelegramAPI
	}
	config := bot{channel: notify.ChannelTelegram, api: api, token: token, name: botName}
	return &Messenger{db: db, bot: config, dialect: &telegram{bot: config, client: newBotClient()}}
}

// telegram — протокол Bot API Telegram: токен в пути, параметры в теле
// запроса, ответ в конверте `{ok, result}`.
type telegram struct {
	bot    bot
	client *http.Client
}

func (t *telegram) Send(ctx context.Context, chatId int64, text string) error {
	return t.call(ctx, "sendMessage", map[string]any{
		"chat_id": chatId,
		"text":    truncateMessage(text, telegramMessageLimit),
	}, nil)
}

func (t *telegram) Updates(ctx context.Context, cursor int64) ([]botUpdate, int64, error) {
	var updates []telegramUpdate
	err := t.call(ctx, "getUpdates", map[string]any{
		"offset":          cursor,
		"timeout":         int(botPollTimeout.Seconds()),
		"allowed_updates": []string{"message"},
	}, &updates)
	if err != nil {
		return nil, cursor, err
	}

	result := make([]botUpdate, 0, len(updates))
	for _, update := range updates {
		// Смещение подтверждает обработку: Bot API повторяет
		// неподтверждённые обновления бесконечно.
		cursor = update.UpdateId + 1
		if update.Message == nil {
			continue
		}
		result = append(result, botUpdate{
			ChatId: update.Message.Chat.Id,
			Text:   update.Message.Text,
		})
	}
	return result, cursor, nil
}

// BindingLink собирает ссылку привязки. Telegram передаёт полезную
// нагрузку параметром start.
func (t *telegram) BindingLink(code string) string {
	if t.bot.name == "" {
		return ""
	}
	return fmt.Sprintf("https://t.me/%s?start=%s", t.bot.name, code)
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

// telegramError — отказ, о котором сообщил сам Bot API.
type telegramError struct {
	Code        int
	Description string
}

func (e *telegramError) Error() string {
	return fmt.Sprintf("telegram api error %d: %s", e.Code, e.Description)
}

func (t *telegram) call(ctx context.Context, method string, params map[string]any, result any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("encoding %s request: %w", method, err)
	}

	url := fmt.Sprintf("%s/bot%s/%s", t.bot.api, t.bot.token, method)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating %s request: %w", method, err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := t.client.Do(request)
	if err != nil {
		return fmt.Errorf("%w: calling %s: %w", ErrChannelUnavailable, method, err)
	}
	defer func() {
		// Тело читается до конца ниже; здесь остаётся только закрыть.
		_ = response.Body.Close()
	}()

	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%w: reading %s response: %w", ErrChannelUnavailable, method, err)
	}

	var envelope struct {
		Ok          bool            `json:"ok"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err = json.Unmarshal(payload, &envelope); err != nil {
		return fmt.Errorf("%w: decoding %s response: %w", ErrChannelUnavailable, method, err)
	}
	if !envelope.Ok {
		apiErr := &telegramError{Code: envelope.ErrorCode, Description: envelope.Description}
		if apiErr.Code == 0 {
			apiErr.Code = response.StatusCode
		}
		switch apiErr.Code {
		case http.StatusForbidden:
			// Пользователь заблокировал бота.
			return fmt.Errorf("%w: %w", ErrChannelBlocked, apiErr)
		case http.StatusBadRequest:
			// Чат не найден или удалён: повторять нечего.
			return fmt.Errorf("%w: %w", ErrChannelUnbound, apiErr)
		default:
			// Ограничение частоты и сбои самого Bot API имеет смысл
			// повторить позже.
			return fmt.Errorf("%w: %w", ErrChannelUnavailable, apiErr)
		}
	}

	if result != nil {
		if err = json.Unmarshal(envelope.Result, result); err != nil {
			return fmt.Errorf("decoding %s result: %w", method, err)
		}
	}
	return nil
}
