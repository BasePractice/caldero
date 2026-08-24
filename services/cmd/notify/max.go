package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"wish/services/shared/notify"
)

// MaxAPI — база адресов Bot API МАКС.
//
// Именно platform-api2: прежний platform-api.max.ru объявлен недействующим,
// и официальные клиенты площадки ходят на второй. Вынесена, чтобы тест мог
// подставить свой сервер.
const MaxAPI = "https://platform-api2.max.ru"

// maxMessageLimit — предел на длину текста сообщения (NewMessageBody.text
// в схеме Bot API). У Telegram он другой, 4096, и один общий предел
// молча резал бы сообщения не там.
const maxMessageLimit = 4000

// Типы обновлений МАКС, которые нужны привязке. Остальные события
// (правка и удаление сообщений, изменения в чатах) приходят в том же
// потоке и пропускаются.
const (
	maxMessageCreated = "message_created"
	maxBotStarted     = "bot_started"
	maxBotStopped     = "bot_stopped"
)

// NewMax собирает бота МАКС.
func NewMax(db Database, token, api, botName string) *Messenger {
	if api == "" {
		api = MaxAPI
	}
	config := bot{channel: notify.ChannelMax, api: api, token: token, name: botName}
	return &Messenger{db: db, bot: config, dialect: &maxBot{bot: config, client: newBotClient()}}
}

// maxBot — протокол Bot API МАКС.
//
// От Telegram он отличается не именами полей, а устройством: токен идёт
// заголовком Authorization (передача его параметром запроса площадкой
// прекращена), получатель — параметром строки запроса, обновления
// забираются методом GET, а ответ приходит без конверта — успех виден
// по коду ответа HTTP.
type maxBot struct {
	bot    bot
	client *http.Client
}

func (m *maxBot) Send(ctx context.Context, chatId int64, text string) error {
	return m.call(ctx, http.MethodPost, "/messages",
		url.Values{"chat_id": {strconv.FormatInt(chatId, 10)}},
		map[string]any{"text": truncateMessage(text, maxMessageLimit)}, nil)
}

func (m *maxBot) Updates(ctx context.Context, cursor int64) ([]botUpdate, int64, error) {
	query := url.Values{"timeout": {strconv.Itoa(int(botPollTimeout.Seconds()))}}
	if cursor > 0 {
		// Метка подтверждает обработку: без неё площадка отдаёт всё,
		// что накопилось с прошлого подтверждения. Нулевая метка —
		// это первый запрос, и передавать её нельзя.
		query.Set("marker", strconv.FormatInt(cursor, 10))
	}

	// Список нужных типов обновлений не передаётся: схема описывает
	// параметр types как список, но не задаёт однозначно, разделять его
	// запятой или повторять параметр. Гадать незачем — лишние события
	// отсеиваются здесь же, ниже.
	var page struct {
		Updates []maxUpdate `json:"updates"`
		// Метка следующей страницы. Может прийти пустой — тогда
		// подтверждать нечего и курсор остаётся прежним.
		Marker *int64 `json:"marker"`
	}
	if err := m.call(ctx, http.MethodGet, "/updates", query, nil, &page); err != nil {
		return nil, cursor, err
	}
	if page.Marker != nil {
		cursor = *page.Marker
	}

	result := make([]botUpdate, 0, len(page.Updates))
	for _, update := range page.Updates {
		switch update.UpdateType {
		case maxMessageCreated:
			// Сообщения ботов пропускаются, и своё в том числе: ответ
			// на собственное сообщение — это бесконечная переписка
			// бота с самим собой.
			if update.Message == nil || update.Message.Sender.IsBot {
				continue
			}
			result = append(result, botUpdate{
				ChatId: update.Message.Recipient.ChatId,
				Text:   update.Message.Body.Text,
			})
		case maxBotStarted:
			// Код привязки приходит не сообщением, а полем payload
			// диплинка. Команда собирается здесь, чтобы привязка была
			// написана один раз и не зависела от площадки.
			text := "/start"
			if update.Payload != "" {
				text += " " + update.Payload
			}
			result = append(result, botUpdate{ChatId: update.ChatId, Text: text, Started: true})
		case maxBotStopped:
			result = append(result, botUpdate{ChatId: update.ChatId, Stopped: true})
		}
	}
	return result, cursor, nil
}

// BindingLink собирает ссылку привязки. МАКС передаёт полезную нагрузку
// частью пути, а не параметром запроса, как Telegram.
func (m *maxBot) BindingLink(code string) string {
	if m.bot.name == "" {
		return ""
	}
	return fmt.Sprintf("https://max.ru/%s/start/%s", m.bot.name, code)
}

// maxUpdate — обновление в том виде, в каком его отдаёт /updates. Событие
// определяется полем update_type, а чат лежит в разных местах: у сообщения
// это получатель внутри самого сообщения, у запуска и остановки бота —
// поле chat_id верхнего уровня.
type maxUpdate struct {
	UpdateType string `json:"update_type"`
	ChatId     int64  `json:"chat_id"`
	Payload    string `json:"payload"`
	Message    *struct {
		Sender struct {
			IsBot bool `json:"is_bot"`
		} `json:"sender"`
		Recipient struct {
			ChatId int64 `json:"chat_id"`
		} `json:"recipient"`
		Body struct {
			Text string `json:"text"`
		} `json:"body"`
	} `json:"message"`
}

// maxError — отказ, о котором сообщил сам Bot API. Тело ошибки —
// `{"code": ..., "message": ...}`, а разряд отказа виден по коду ответа.
type maxError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *maxError) Error() string {
	return fmt.Sprintf("max api error %d (%s): %s", e.Status, e.Code, e.Message)
}

func (m *maxBot) call(
	ctx context.Context,
	method, path string,
	query url.Values,
	params map[string]any,
	result any,
) error {
	var body io.Reader
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("encoding %s request: %w", path, err)
		}
		body = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, m.bot.api+path+"?"+query.Encode(), body)
	if err != nil {
		return fmt.Errorf("creating %s request: %w", path, err)
	}
	// Токен идёт заголовком без схемы: именно так его ждёт площадка.
	request.Header.Set("Authorization", m.bot.token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := m.client.Do(request)
	if err != nil {
		return fmt.Errorf("%w: calling %s: %w", ErrChannelUnavailable, path, err)
	}
	defer func() {
		// Тело читается до конца ниже; здесь остаётся только закрыть.
		_ = response.Body.Close()
	}()

	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%w: reading %s response: %w", ErrChannelUnavailable, path, err)
	}

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return m.failure(response.StatusCode, payload)
	}
	if result != nil {
		if err = json.Unmarshal(payload, result); err != nil {
			return fmt.Errorf("%w: decoding %s response: %w", ErrChannelUnavailable, path, err)
		}
	}
	return nil
}

// failure разбирает отказ и делит отказы по тому, что с ними делать.
func (m *maxBot) failure(status int, payload []byte) error {
	apiErr := &maxError{Status: status}
	// Тело ошибки может и не разобраться: тогда остаётся код ответа.
	_ = json.Unmarshal(payload, apiErr)

	switch status {
	case http.StatusForbidden:
		// Пользователь остановил бота или доступа к чату нет.
		return fmt.Errorf("%w: %w", ErrChannelBlocked, apiErr)
	case http.StatusBadRequest, http.StatusNotFound:
		// Чата нет или запрос к нему неверен: повторять нечего.
		return fmt.Errorf("%w: %w", ErrChannelUnbound, apiErr)
	default:
		// Ограничение частоты (у площадки это 30 запросов в секунду)
		// и её собственные сбои имеет смысл повторить позже. Сюда же
		// попадает неверный токен: повтор не поможет, но пользователь
		// тут ни при чём, и отметка о блокировке была бы враньём.
		return fmt.Errorf("%w: %w", ErrChannelUnavailable, apiErr)
	}
}
