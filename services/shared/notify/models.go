// Package notify описывает модель оповещений: события, каналы доставки
// и настройки пользователя. Пакет общий: события публикуют другие сервисы,
// и типы событий должны быть у них перед глазами, а не в чужом коде.
package notify

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// EventType — что произошло. Тип события, а не готовый текст: формулировка
// живёт в шаблоне и меняется без изменения кода публикующего сервиса.
type EventType string

const (
	// EventWishlistItemReserved — элемент списка желаний зарезервирован
	// дарителем.
	EventWishlistItemReserved EventType = "WISHLIST_ITEM_RESERVED"
	// EventWishlistItemGifted — подарок вручён.
	EventWishlistItemGifted EventType = "WISHLIST_ITEM_GIFTED"
	// EventWishlistItemConfirmed — одаряемый согласился принять подарок.
	// Адресуется дарителю: это ему решать, что делать дальше.
	EventWishlistItemConfirmed EventType = "WISHLIST_ITEM_CONFIRMED"
	// EventWishlistItemRejected — одаряемый отказался от подарка.
	EventWishlistItemRejected EventType = "WISHLIST_ITEM_REJECTED"
	// EventCaldronMemberAdded — пользователя добавили в котёл.
	EventCaldronMemberAdded EventType = "CALDRON_MEMBER_ADDED"
	// EventCaldronStateChanged — котёл сменил состояние.
	EventCaldronStateChanged EventType = "CALDRON_STATE_CHANGED"
	// EventCaldronDrawResult — итог розыгрыша.
	EventCaldronDrawResult EventType = "CALDRON_DRAW_RESULT"
	// EventPaymentSettled — платёж завершён.
	EventPaymentSettled EventType = "PAYMENT_SETTLED"
	// EventConfirmationCode — код подтверждения телефона или почты.
	EventConfirmationCode EventType = "CONFIRMATION_CODE"
	// EventConfirmationLink — ссылка подтверждения почты. Токен длиной
	// в тридцать два байта руками не переписывают, поэтому у почты
	// отдельное сообщение со ссылкой.
	EventConfirmationLink EventType = "CONFIRMATION_LINK"
)

// EventTypes перечисляет известные типы событий.
func EventTypes() []EventType {
	return []EventType{
		EventWishlistItemReserved,
		EventWishlistItemGifted,
		EventWishlistItemConfirmed,
		EventWishlistItemRejected,
		EventCaldronMemberAdded,
		EventCaldronStateChanged,
		EventCaldronDrawResult,
		EventPaymentSettled,
		EventConfirmationCode,
		EventConfirmationLink,
	}
}

// Valid сообщает, известен ли тип события.
func (t EventType) Valid() bool {
	for _, known := range EventTypes() {
		if known == t {
			return true
		}
	}
	return false
}

// Channel — куда доставлять.
//
// WebSocket и длинный опрос отдельными каналами не считаются: это два
// способа забрать одно и то же сообщение из приложения. Развели бы их —
// пользователь с открытой вкладкой и работающим опросом получил бы
// каждое событие дважды.
type Channel string

const (
	// ChannelInApp — сообщение в приложении: доставляется по WebSocket
	// активной сессии и остаётся в ленте для длинного опроса.
	ChannelInApp Channel = "IN_APP"
	// ChannelTelegram — сообщение ботом в Telegram.
	ChannelTelegram Channel = "TELEGRAM"
)

// Channels перечисляет известные каналы.
func Channels() []Channel {
	return []Channel{ChannelInApp, ChannelTelegram}
}

func (c Channel) Valid() bool {
	for _, known := range Channels() {
		if known == c {
			return true
		}
	}
	return false
}

// Event — то, что произошло, и кому об этом сообщить.
type Event struct {
	Id     uuid.UUID `json:"id"`
	UserId uuid.UUID `json:"user_id"`
	Type   EventType `json:"type"`
	// Payload — подстановки для шаблона. Плоская карта строк, а не
	// произвольная структура: шаблон не должен знать типов публикующего
	// сервиса, а лог события — содержать неизвестно что.
	Payload map[string]string `json:"payload,omitempty"`
	// DedupKey отсекает повтор. Публикующий сервис может прислать событие
	// дважды — при ретрае или при повторной обработке своей очереди, —
	// и без ключа пользователь получит два одинаковых сообщения.
	DedupKey  string    `json:"dedup_key,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// PublishEvent — запрос на публикацию события.
type PublishEvent struct {
	UserId  uuid.UUID         `json:"user_id"`
	Type    EventType         `json:"type"`
	Payload map[string]string `json:"payload,omitempty"`
	// DedupKey необязателен: без него повторная публикация создаст
	// второе сообщение.
	DedupKey string `json:"dedup_key,omitempty"`
}

// MaxPayloadEntries ограничивает размер подстановок: событие приходит
// из другого сервиса, и без ограничения его размер задаёт вызывающий.
const MaxPayloadEntries = 32

// MaxPayloadValue ограничивает длину одного значения.
const MaxPayloadValue = 512

// Validate возвращает причину отказа, а не просто false.
func (p PublishEvent) Validate() error {
	if p.UserId == uuid.Nil {
		return errors.New("user_id is required")
	}
	if !p.Type.Valid() {
		return fmt.Errorf("unknown event type %q", p.Type)
	}
	if len(p.Payload) > MaxPayloadEntries {
		return fmt.Errorf("payload must not exceed %d entries, got %d",
			MaxPayloadEntries, len(p.Payload))
	}
	for key, value := range p.Payload {
		if key == "" {
			return errors.New("payload keys must not be empty")
		}
		if len(value) > MaxPayloadValue {
			return fmt.Errorf("payload value %q exceeds %d characters", key, MaxPayloadValue)
		}
	}
	if len(p.DedupKey) > 128 {
		return fmt.Errorf("dedup_key must not exceed 128 characters, got %d", len(p.DedupKey))
	}
	return nil
}

func (p PublishEvent) String() string {
	return fmt.Sprintf("{user_id=%s, type=%s}", p.UserId, p.Type)
}

// Message — сообщение в ленте приложения.
type Message struct {
	Id uuid.UUID `json:"id"`
	// Seq — курсор ленты. Монотонный и сквозной по пользователю: по нему
	// длинный опрос забирает то, что появилось после последнего просмотра.
	Seq       int64     `json:"seq"`
	Type      EventType `json:"type"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// Preference — настройка доставки: какие события и в какой канал.
type Preference struct {
	Type    EventType `json:"type"`
	Channel Channel   `json:"channel"`
	Enabled bool      `json:"enabled"`
}

func (p Preference) Validate() error {
	if !p.Type.Valid() {
		return fmt.Errorf("unknown event type %q", p.Type)
	}
	if !p.Channel.Valid() {
		return fmt.Errorf("unknown channel %q", p.Channel)
	}
	return nil
}

// DefaultEnabled сообщает, включён ли канал для события, когда пользователь
// ничего не настраивал.
//
// Код подтверждения по умолчанию не уходит в Telegram: до подтверждения
// привязки бот может быть чужим, а код — это доступ к учётной записи.
func DefaultEnabled(eventType EventType, channel Channel) bool {
	if channel != ChannelTelegram {
		return true
	}
	switch eventType {
	case EventConfirmationCode, EventConfirmationLink:
		return false
	default:
		return true
	}
}
