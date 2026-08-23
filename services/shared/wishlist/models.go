// Package wishlist описывает список желаний: элементы, их состояния
// и допустимые переходы. Пакет общий: состояния нужны и сервису котла
// подарков (T-054), и интерфейсу.
package wishlist

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"wish/services/shared/credit"
	"wish/services/shared/marketplace"
)

// State — состояние элемента списка. Набор задан README дословно:
// виден, не виден, выбран, подтверждён, акцептован, отклонён.
type State string

const (
	// StateVisible — элемент виден дарителям и доступен к выбору.
	StateVisible State = "VISIBLE"
	// StateHidden — владелец скрыл элемент; дарителям он не показывается.
	StateHidden State = "HIDDEN"
	// StateChosen — даритель зарезервировал элемент и ждёт решения
	// одаряемого. Другим дарителям элемент уже не виден.
	StateChosen State = "CHOSEN"
	// StateConfirmed — одаряемый согласен принять подарок.
	StateConfirmed State = "CONFIRMED"
	// StateAccepted — подарок вручён: заказан на площадке или переведён
	// деньгами. Состояние терминальное.
	StateAccepted State = "ACCEPTED"
	// StateRejected — одаряемый отказался. Состояние терминальное:
	// по README отклонённый подарок больше не доступен к дарению.
	StateRejected State = "REJECTED"
)

// Actor — кто выполняет переход. Право на переход зависит не только
// от состояния: скрыть элемент может владелец, зарезервировать — даритель,
// а освободить просроченный резерв — только сама система.
type Actor string

const (
	// ActorOwner — владелец списка, он же одаряемый.
	ActorOwner Actor = "OWNER"
	// ActorGiver — даритель, зарезервировавший элемент.
	ActorGiver Actor = "GIVER"
	// ActorSystem — фоновое освобождение просроченных резервов.
	ActorSystem Actor = "SYSTEM"
)

// Terminal сообщает, что состояние окончательное.
func (s State) Terminal() bool {
	return s == StateAccepted || s == StateRejected
}

// Valid сообщает, известно ли состояние.
func (s State) Valid() bool {
	switch s {
	case StateVisible, StateHidden, StateChosen, StateConfirmed, StateAccepted, StateRejected:
		return true
	default:
		return false
	}
}

// transitions — таблица допустимых переходов. Явная таблица, а не набор
// условий по коду: иначе очередное состояние добавляется в одном месте,
// а забывается в трёх.
var transitions = map[State]map[State][]Actor{
	StateVisible: {
		StateHidden: {ActorOwner},
		StateChosen: {ActorGiver},
	},
	StateHidden: {
		StateVisible: {ActorOwner},
	},
	StateChosen: {
		// Одаряемый решает судьбу выбранного подарка.
		StateConfirmed: {ActorOwner},
		StateRejected:  {ActorOwner},
		// Даритель может передумать, а система — освободить резерв,
		// у которого вышел срок: иначе брошенный резерв блокирует
		// подарок навсегда.
		StateVisible: {ActorGiver, ActorSystem},
	},
	StateConfirmed: {
		StateAccepted: {ActorGiver},
		// Отказ дарителя после подтверждения возвращает элемент в список:
		// одаряемый согласился на подарок, а не потерял его.
		StateVisible: {ActorGiver},
	},
}

// ErrForbiddenTransition — переход не разрешён этому участнику.
var ErrForbiddenTransition = errors.New("transition is not allowed for this actor")

// ErrInvalidTransition — такого перехода нет вовсе.
var ErrInvalidTransition = errors.New("invalid state transition")

// CanTransition проверяет переход. Возвращает причину отказа: клиенту
// нужно знать, что именно не так, а не просто «нельзя».
func CanTransition(from, to State, actor Actor) error {
	if !from.Valid() || !to.Valid() {
		return fmt.Errorf("%w: unknown state", ErrInvalidTransition)
	}
	actors, ok := transitions[from][to]
	if !ok {
		return fmt.Errorf("%w: %s cannot become %s", ErrInvalidTransition, from, to)
	}
	for _, allowed := range actors {
		if allowed == actor {
			return nil
		}
	}
	return fmt.Errorf("%w: %s cannot change %s to %s", ErrForbiddenTransition, actor, from, to)
}

// Kind — что именно дарят.
type Kind string

const (
	// KindProduct — товар с площадки.
	KindProduct Kind = "PRODUCT"
	// KindMoney — денежные средства.
	KindMoney Kind = "MONEY"
)

// Приоритет элемента: чем меньше число, тем важнее подарок.
const (
	MinPriority = 1
	MaxPriority = 5
)

// Item — элемент списка желаний.
type Item struct {
	Id     uuid.UUID `json:"id"`
	UserId uuid.UUID `json:"user_id"`
	Kind   Kind      `json:"kind"`
	State  State     `json:"state"`
	// Priority — 1 самый важный, 5 наименее.
	Priority int    `json:"priority"`
	Title    string `json:"title"`
	Comment  string `json:"comment,omitempty"`

	// Товарные поля. Цена — снимок на момент добавления: на площадке
	// она меняется, и показывать её как текущую нельзя.
	Provider  marketplace.Provider `json:"provider,omitempty"`
	ProductId string               `json:"product_id,omitempty"`
	URL       string               `json:"url,omitempty"`
	Price     credit.Amount        `json:"price,omitempty"`
	PriceAt   *time.Time           `json:"price_at,omitempty"`

	// Amount заполняется у денежного элемента.
	Amount credit.Amount `json:"amount,omitempty"`

	// GiverId виден только самому дарителю. Владельцу списка он не
	// показывается: по README одаряемый узнаёт, что «кто-то» хочет
	// вручить подарок, — сюрприз входит в продукт.
	GiverId *uuid.UUID `json:"giver_id,omitempty"`
	// ReservedUntil — до какого момента держится резерв.
	ReservedUntil *time.Time `json:"reserved_until,omitempty"`
	// OrderId — номер заказа на площадке, если заказ удалось оформить.
	OrderId string `json:"order_id,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CreateItem — запрос на добавление элемента.
type CreateItem struct {
	Kind     Kind                 `json:"kind"`
	Priority int                  `json:"priority"`
	Comment  string               `json:"comment,omitempty"`
	Provider marketplace.Provider `json:"provider,omitempty"`
	// ProductId — идентификатор товара на площадке. Название и цена
	// не принимаются от клиента: их берёт сам сервис из карточки,
	// иначе в списке окажется товар с выдуманной ценой.
	ProductId string        `json:"product_id,omitempty"`
	Amount    credit.Amount `json:"amount,omitempty"`
	// Title нужен только денежному элементу: «на новый велосипед».
	Title string `json:"title,omitempty"`
}

// MaxTitle и MaxComment ограничивают текст, приходящий от пользователя.
const (
	MaxTitle   = 200
	MaxComment = 1000
)

func (c CreateItem) Validate() error {
	if c.Priority < MinPriority || c.Priority > MaxPriority {
		return fmt.Errorf("priority must be between %d and %d, got %d",
			MinPriority, MaxPriority, c.Priority)
	}
	if len([]rune(c.Comment)) > MaxComment {
		return fmt.Errorf("comment must not exceed %d characters", MaxComment)
	}
	if len([]rune(c.Title)) > MaxTitle {
		return fmt.Errorf("title must not exceed %d characters", MaxTitle)
	}

	switch c.Kind {
	case KindProduct:
		if c.ProductId == "" {
			return errors.New("product_id is required for a product item")
		}
		if c.Provider == "" {
			return errors.New("provider is required for a product item")
		}
		if c.Amount != 0 {
			return errors.New("amount must be empty for a product item")
		}
		return nil
	case KindMoney:
		if c.Amount <= 0 {
			return fmt.Errorf("amount must be positive, got %d", c.Amount)
		}
		if c.ProductId != "" || c.Provider != "" {
			return errors.New("product fields must be empty for a money item")
		}
		if c.Title == "" {
			return errors.New("title is required for a money item")
		}
		return nil
	default:
		return fmt.Errorf("kind must be one of %s, %s", KindProduct, KindMoney)
	}
}

func (c CreateItem) String() string {
	return fmt.Sprintf("{kind=%s, priority=%d}", c.Kind, c.Priority)
}

// Public скрывает от чужих глаз то, что им знать не положено.
//
// Даритель не должен видеть, кто ещё смотрит список; владелец не должен
// видеть, кто именно выбрал его подарок. Поэтому решение принимается здесь,
// а не в каждом обработчике отдельно.
func (i Item) Public(viewer uuid.UUID) Item {
	if i.GiverId != nil && *i.GiverId != viewer {
		i.GiverId = nil
		i.ReservedUntil = nil
	}
	return i
}
