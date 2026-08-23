package payment

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"wish/services/shared/credit"
)

// Ошибки обработки вебхука.
var (
	// ErrInvalidSignature — подпись не сходится. Повторять такой вебхук
	// бессмысленно: либо секрет не тот, либо тело подменили.
	ErrInvalidSignature = errors.New("webhook signature does not match")
	// ErrStaleSignature — отметка времени вне допустимого окна. Без этой
	// проверки перехваченный вебхук можно переслать повторно спустя сутки.
	ErrStaleSignature = errors.New("webhook timestamp is outside the tolerance window")
	// ErrEventIgnored — событие получено, но менять нечего: оно уже
	// применялось, устарело или не меняет состояния. Для провайдера это
	// успех, а не ошибка, иначе он будет повторять доставку бесконечно.
	ErrEventIgnored = errors.New("webhook event does not change the operation")
)

// Event — событие о судьбе операции, присланное провайдером.
type Event struct {
	// Id — идентификатор события у провайдера. По нему отсекаются повторы:
	// провайдер доставляет вебхук «хотя бы один раз», а не «ровно один раз».
	Id string `json:"id"`
	// OperationId — операция, к которой относится событие.
	OperationId string   `json:"operation_id"`
	Provider    Provider `json:"provider"`
	Status      Status   `json:"status"`
	// Amount — сумма по данным провайдера. Расхождение с суммой операции
	// означает не округление, а чужое или подменённое событие.
	Amount        credit.Amount `json:"amount"`
	FailureReason string        `json:"failure_reason,omitempty"`
	// OccurredAt — когда событие произошло у провайдера. Порядок доставки
	// вебхуков не гарантирован, и время события — единственный признак,
	// по которому можно отличить опоздавшее событие от нового.
	OccurredAt time.Time `json:"occurred_at"`
}

func (e Event) Validate() error {
	if e.Id == "" {
		return errors.New("event id is required")
	}
	if e.OperationId == "" {
		return errors.New("operation_id is required")
	}
	switch e.Status {
	case StatusPending, StatusSucceeded, StatusFailed:
	default:
		return fmt.Errorf("unknown status %q", e.Status)
	}
	if e.OccurredAt.IsZero() {
		return errors.New("occurred_at is required")
	}
	return nil
}

// ApplyTo вычисляет новое состояние операции. Чистая функция: решение
// о переходе не зависит ни от хранилища, ни от транспорта.
//
// Возвращает ErrEventIgnored, если применять нечего — это штатный исход,
// а не сбой.
func (e Event) ApplyTo(operation Operation) (Operation, error) {
	if operation.Id != e.OperationId {
		return operation, fmt.Errorf("event %s belongs to operation %s, not %s",
			e.Id, e.OperationId, operation.Id)
	}
	// Сумма сверяется всегда: событие с чужой суммой — это либо ошибка
	// на стороне провайдера, либо подмена, и молча проводить его нельзя.
	if e.Amount != 0 && e.Amount != operation.Amount {
		return operation, fmt.Errorf("event %s reports amount %d, operation holds %d",
			e.Id, e.Amount, operation.Amount)
	}
	if operation.Status.Terminal() {
		// Терминальное состояние не пересматривается: опоздавший вебхук
		// иначе отменил бы уже проведённый платёж.
		return operation, fmt.Errorf("%w: operation is already %s",
			ErrEventIgnored, operation.Status)
	}
	if e.Status == operation.Status {
		return operation, fmt.Errorf("%w: status is unchanged", ErrEventIgnored)
	}
	if !operation.Status.CanFollow(e.Status) {
		return operation, fmt.Errorf("%w: %s cannot follow %s",
			ErrEventIgnored, e.Status, operation.Status)
	}
	if !operation.UpdatedAt.IsZero() && e.OccurredAt.Before(operation.UpdatedAt) {
		return operation, fmt.Errorf("%w: event is older than the operation state",
			ErrEventIgnored)
	}

	updated := operation
	updated.Status = e.Status
	updated.FailureReason = e.FailureReason
	updated.UpdatedAt = e.OccurredAt
	return updated, nil
}

// OperationStore — состояние платёжных операций. Интерфейс объявлен здесь,
// у потребителя: хранилищу незачем знать правила переходов.
type OperationStore interface {
	// Apply применяет событие к операции одной транзакцией: находит
	// операцию, вызывает transition и сохраняет результат вместе с отметкой
	// об обработанном событии.
	//
	// Разделять чтение и запись нельзя: два вебхука одной операции,
	// пришедшие одновременно, оба увидели бы её незавершённой и оба
	// перевели бы её в терминальное состояние — то есть провели бы платёж
	// дважды. По той же причине проверка повтора события выполняется здесь,
	// а не отдельным запросом до вызова.
	//
	// Повторное событие и отказ transition возвращают ErrEventIgnored.
	Apply(ctx context.Context, event Event, transition func(Operation) (Operation, error)) (Operation, error)
}

// Verifier проверяет подпись вебхука.
//
// Схема — HMAC-SHA256 от отметки времени и тела: она не требует от провайдера
// ничего, кроме общего секрета, и повторяется у большинства из них. Конкретный
// формат заголовка у провайдера свой, поэтому разбор заголовков остаётся
// снаружи, а здесь только проверка.
type Verifier struct {
	secret    []byte
	tolerance time.Duration
}

// DefaultWebhookTolerance — допустимое расхождение часов и задержка доставки.
const DefaultWebhookTolerance = 5 * time.Minute

func NewVerifier(secret string, tolerance time.Duration) (*Verifier, error) {
	if secret == "" {
		return nil, errors.New("webhook secret is required")
	}
	if tolerance <= 0 {
		tolerance = DefaultWebhookTolerance
	}
	return &Verifier{secret: []byte(secret), tolerance: tolerance}, nil
}

// Verify сверяет подпись тела и свежесть отметки времени.
func (v *Verifier) Verify(body []byte, timestamp time.Time, signature string) error {
	if timestamp.IsZero() {
		return fmt.Errorf("%w: timestamp is missing", ErrStaleSignature)
	}
	if drift := time.Since(timestamp); drift > v.tolerance || drift < -v.tolerance {
		return fmt.Errorf("%w: drift %s", ErrStaleSignature, drift.Round(time.Second))
	}

	expected, err := hex.DecodeString(signature)
	if err != nil {
		return fmt.Errorf("%w: signature is not hex", ErrInvalidSignature)
	}
	// hmac.Equal, а не ==: сравнение с ранним выходом даёт возможность
	// подобрать подпись побайтно по времени ответа.
	if !hmac.Equal(expected, v.sign(body, timestamp)) {
		return ErrInvalidSignature
	}
	return nil
}

// Sign возвращает подпись в том же виде, в каком её ожидает Verify.
// Нужна отправляющей стороне: песочнице и тестам.
func (v *Verifier) Sign(body []byte, timestamp time.Time) string {
	return hex.EncodeToString(v.sign(body, timestamp))
}

func (v *Verifier) sign(body []byte, timestamp time.Time) []byte {
	mac := hmac.New(sha256.New, v.secret)
	// Ошибки записи игнорируются явно: hash.Hash по контракту не возвращает
	// их никогда, и проверять здесь нечего.
	// Отметка времени входит в подпись: иначе её можно подменить,
	// оставив тело и подпись прежними, и проверка окна ничего не даст.
	_, _ = mac.Write([]byte(strconv.FormatInt(timestamp.Unix(), 10)))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(body)
	return mac.Sum(nil)
}
