// Package payment описывает работу с внешним платёжным провайдером:
// пополнение кошелька, вывод средств и привязку карт.
package payment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"wish/services/shared/credit"
)

// Ошибки, общие для всех провайдеров.
var (
	// ErrUnavailable — провайдер недоступен. Отличается от отказа в платеже:
	// размыкать цепь имеет смысл только на недоступность.
	ErrUnavailable = errors.New("payment provider is unavailable")
	// ErrRejected — провайдер отклонил операцию. Это нормальный ответ,
	// а не сбой зависимости.
	ErrRejected = errors.New("payment is rejected by the provider")
	// ErrNotFound — операции с таким идентификатором у провайдера нет.
	ErrNotFound = errors.New("payment operation not found")
	// ErrUnsupported — провайдер не поддерживает способ или операцию.
	// Выплата на карту доступна не у всех, а привязка карт — не всегда.
	ErrUnsupported = errors.New("operation is not supported by the payment provider")
)

// Provider — идентификатор платёжного провайдера.
type Provider string

const (
	// ProviderSandbox — песочница для локальной разработки и тестов.
	ProviderSandbox Provider = "SANDBOX"
)

// Method — способ проведения денег.
type Method string

const (
	// MethodSBP — Система быстрых платежей: по номеру телефона, без карты.
	MethodSBP Method = "SBP"
	// MethodCard — карта, привязанная у провайдера. В системе хранится
	// только токен, номер карты не появляется нигде, включая логи.
	MethodCard Method = "CARD"
)

// Direction — направление движения денег относительно системы.
type Direction string

const (
	// DirectionDeposit — пополнение: деньги приходят в систему.
	DirectionDeposit Direction = "DEPOSIT"
	// DirectionPayout — выплата: деньги уходят из системы.
	DirectionPayout Direction = "PAYOUT"
)

// Status — состояние операции у провайдера.
//
// Модель намеренно монотонная: из Pending можно уйти в Succeeded или Failed,
// а терминальное состояние не меняется никогда. Вебхуки провайдера приходят
// с повторами и не в том порядке, в котором произошли события, и без этого
// правила поздний «pending» отменил бы уже проведённый платёж.
type Status string

const (
	StatusPending   Status = "PENDING"
	StatusSucceeded Status = "SUCCEEDED"
	StatusFailed    Status = "FAILED"
)

// Terminal сообщает, что состояние окончательное.
func (s Status) Terminal() bool {
	return s == StatusSucceeded || s == StatusFailed
}

// CanFollow сообщает, допустим ли переход в состояние next.
func (s Status) CanFollow(next Status) bool {
	if s.Terminal() {
		return false
	}
	return next == StatusPending || next.Terminal()
}

// Operation — денежная операция у провайдера.
type Operation struct {
	Provider Provider  `json:"provider"`
	Id       string    `json:"id"`
	UserId   uuid.UUID `json:"user_id"`
	// IdempotencyKey задаётся системой и передаётся провайдеру: повтор
	// запроса после таймаута не должен создать второй платёж.
	IdempotencyKey string        `json:"idempotency_key"`
	Direction      Direction     `json:"direction"`
	Method         Method        `json:"method"`
	Status         Status        `json:"status"`
	Amount         credit.Amount `json:"amount"`
	// Fee — комиссия, удержанная сверх суммы операции.
	Fee credit.Amount `json:"fee"`
	// ConfirmationURL — куда отправить пользователя для подтверждения
	// пополнения. У выплаты пустой.
	ConfirmationURL string `json:"confirmation_url,omitempty"`
	// FailureReason заполняется только у StatusFailed.
	FailureReason string    `json:"failure_reason,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// DepositRequest — пополнение кошелька.
type DepositRequest struct {
	IdempotencyKey string
	UserId         uuid.UUID
	Amount         credit.Amount
	Method         Method
	Description    string
	// ReturnURL — куда вернуть пользователя после оплаты.
	ReturnURL string
}

// Validate возвращает причину отказа, а не просто false.
func (r DepositRequest) Validate() error {
	if r.IdempotencyKey == "" {
		return errors.New("idempotency_key is required")
	}
	if r.UserId == uuid.Nil {
		return errors.New("user_id is required")
	}
	if err := validAmount(r.Amount); err != nil {
		return err
	}
	if r.Method != MethodSBP && r.Method != MethodCard {
		return fmt.Errorf("method must be one of %s, %s", MethodSBP, MethodCard)
	}
	return nil
}

// PayoutRequest — вывод средств.
type PayoutRequest struct {
	IdempotencyKey string
	UserId         uuid.UUID
	Amount         credit.Amount
	Method         Method
	// Phone — получатель для СБП в формате E.164.
	Phone string
	// Bank — идентификатор банка получателя в СБП. Пустое значение
	// означает банк по умолчанию, выбранный получателем.
	Bank string
	// CardToken — токен карты у провайдера для MethodCard.
	CardToken string
}

func (r PayoutRequest) Validate() error {
	if r.IdempotencyKey == "" {
		return errors.New("idempotency_key is required")
	}
	if r.UserId == uuid.Nil {
		return errors.New("user_id is required")
	}
	if err := validAmount(r.Amount); err != nil {
		return err
	}
	switch r.Method {
	case MethodSBP:
		if r.Phone == "" {
			return fmt.Errorf("phone is required for %s payout", MethodSBP)
		}
	case MethodCard:
		if r.CardToken == "" {
			return fmt.Errorf("card_token is required for %s payout", MethodCard)
		}
	default:
		return fmt.Errorf("method must be one of %s, %s", MethodSBP, MethodCard)
	}
	return nil
}

// validAmount проверяет сумму операции. Верхняя граница нужна расчёту
// комиссии: без неё произведение суммы на ставку переполняет int64.
func validAmount(amount credit.Amount) error {
	if amount <= 0 {
		return fmt.Errorf("amount must be positive, got %d", amount)
	}
	if amount > MaxAmount {
		return fmt.Errorf("amount %d exceeds the limit of %d", amount, MaxAmount)
	}
	return nil
}

// Card — привязанная карта. Хранится только то, что можно показать
// пользователю: полного номера в системе нет ни в базе, ни в логах.
type Card struct {
	Token    string    `json:"token"`
	Last4    string    `json:"last4"`
	Brand    string    `json:"brand"`
	ExpMonth int       `json:"exp_month"`
	ExpYear  int       `json:"exp_year"`
	BoundAt  time.Time `json:"bound_at"`
}

// Binding — начатая привязка карты. Данные карты пользователь вводит
// на стороне провайдера, система их не видит.
type Binding struct {
	Token           string `json:"token"`
	ConfirmationURL string `json:"confirmation_url"`
}

// Gateway — то, что нужно от платёжного провайдера. Интерфейс объявлен
// здесь, у потребителя: провайдеру незачем знать, кто им пользуется.
type Gateway interface {
	// Deposit создаёт пополнение и возвращает операцию в состоянии Pending:
	// деньги приходят не в момент запроса, а после подтверждения.
	Deposit(ctx context.Context, request DepositRequest) (Operation, error)
	// Payout создаёт выплату.
	Payout(ctx context.Context, request PayoutRequest) (Operation, error)
	// Status запрашивает состояние операции у провайдера. Нужен для сверки:
	// вебхук может не дойти, и тогда операция зависнет в Pending навсегда.
	Status(ctx context.Context, id string) (Operation, error)
	// Provider сообщает, какой это провайдер.
	Provider() Provider
}

// CardVault — привязка карт. Вынесена из Gateway отдельным интерфейсом,
// потому что поддерживается не всяким провайдером, и приведение типа
// честнее, чем метод, всегда возвращающий ErrUnsupported.
type CardVault interface {
	// Bind начинает привязку и возвращает адрес формы провайдера.
	Bind(ctx context.Context, user uuid.UUID) (Binding, error)
	// Card возвращает привязанную карту по токену.
	Card(ctx context.Context, token string) (Card, error)
	// Unbind отвязывает карту.
	Unbind(ctx context.Context, token string) error
}
