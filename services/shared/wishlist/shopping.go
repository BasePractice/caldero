package wishlist

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"wish/services/shared/credit"
	"wish/services/shared/marketplace"
)

// RunState — чем закончился прогон шопоголика.
type RunState string

const (
	// RunDone — заказано всё, что отобрано.
	RunDone RunState = "DONE"
	// RunPartial — часть заказов не прошла. Списано только за то,
	// что удалось заказать.
	RunPartial RunState = "PARTIAL"
	// RunEmpty — в бюджет не поместилось ничего, средства не тронуты.
	RunEmpty RunState = "EMPTY"
)

// MaxShoppingItems ограничивает список кандидатов: он приходит от клиента,
// и каждый элемент — это запрос к площадке.
const MaxShoppingItems = 30

// StartShopping — запрос на прогон шопоголика.
type StartShopping struct {
	// Budget — сколько разрешено потратить. Сумма заказа не превысит его
	// ни при каких обстоятельствах.
	Budget credit.Amount `json:"budget"`
	// Items — из чего выбирать. Что именно попадёт в заказ, решает
	// случайный отбор.
	Items []ShoppingItem `json:"items"`
}

// ShoppingItem — товар-кандидат.
type ShoppingItem struct {
	Provider  marketplace.Provider `json:"provider"`
	ProductId string               `json:"product_id"`
}

func (s StartShopping) Validate() error {
	if s.Budget <= 0 {
		return fmt.Errorf("budget must be positive, got %d", s.Budget)
	}
	if len(s.Items) == 0 {
		return errors.New("items are required")
	}
	if len(s.Items) > MaxShoppingItems {
		return fmt.Errorf("items must not exceed %d, got %d", MaxShoppingItems, len(s.Items))
	}
	for _, item := range s.Items {
		if item.Provider == "" || item.ProductId == "" {
			return errors.New("each item needs provider and product_id")
		}
	}
	return nil
}

func (s StartShopping) String() string {
	return fmt.Sprintf("{budget=%s, items=%d}", s.Budget, len(s.Items))
}

// Run — прогон шопоголика вместе с его итогом.
type Run struct {
	Id     uuid.UUID     `json:"id"`
	UserId uuid.UUID     `json:"user_id"`
	Budget credit.Amount `json:"budget"`
	// Spent — сколько фактически списано. Отличается от стоимости
	// отобранного набора, если часть заказов не прошла.
	Spent credit.Amount `json:"spent"`
	State RunState      `json:"state"`
	// Seed — зерно отбора. Хранится, чтобы результат можно было объяснить:
	// по нему виден тот же набор, что выпал пользователю.
	Seed      string     `json:"seed,omitempty"`
	Purchases []Purchase `json:"purchases,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// Purchase — отобранный товар и его судьба.
type Purchase struct {
	Id        uuid.UUID            `json:"id"`
	RunId     uuid.UUID            `json:"run_id"`
	Provider  marketplace.Provider `json:"provider"`
	ProductId string               `json:"product_id"`
	Title     string               `json:"title"`
	URL       string               `json:"url,omitempty"`
	Price     credit.Amount        `json:"price"`
	// Ordered и Paid разделены намеренно: заказ и оплата — разные шаги,
	// и при сбое между ними нужно видеть, что именно не состоялось.
	Ordered bool   `json:"ordered"`
	Paid    bool   `json:"paid"`
	OrderId string `json:"order_id,omitempty"`
	// Failure объясняет, почему товар не заказан.
	Failure   string    `json:"failure,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}
