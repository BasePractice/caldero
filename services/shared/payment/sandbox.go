package payment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Sandbox — платёжный провайдер для локальной разработки и тестов.
//
// Существует потому, что без него разработка платёжного контура упирается
// в договор с платёжным агентом (EXT-01). Идентификаторы производятся
// из ключа идемпотентности детерминированно: один и тот же ключ всегда
// даёт одну и ту же операцию, иначе тесты становятся невоспроизводимыми.
//
// Собственное состояние Sandbox держит отдельно от хранилища системы:
// сверка (Reconciler) имеет смысл только тогда, когда эти два состояния
// действительно могут разойтись.
type Sandbox struct {
	mu          sync.Mutex
	operations  map[string]Operation
	byKey       map[string]string
	cards       map[string]Card
	unavailable bool

	// Fee — тариф песочницы.
	Fee Fee
	// CardsSupported отражает то, что привязка карт есть не у всякого
	// провайдера.
	CardsSupported bool
}

func NewSandbox() *Sandbox {
	return &Sandbox{
		operations:     make(map[string]Operation),
		byKey:          make(map[string]string),
		cards:          make(map[string]Card),
		CardsSupported: true,
	}
}

func (s *Sandbox) Provider() Provider {
	return ProviderSandbox
}

// SetUnavailable имитирует недоступность провайдера.
func (s *Sandbox) SetUnavailable(unavailable bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unavailable = unavailable
}

func (s *Sandbox) Deposit(_ context.Context, request DepositRequest) (Operation, error) {
	if err := request.Validate(); err != nil {
		return Operation{}, fmt.Errorf("%w: %w", ErrRejected, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unavailable {
		return Operation{}, ErrUnavailable
	}
	if existing, ok := s.byIdempotencyKey(request.IdempotencyKey); ok {
		return existing, nil
	}

	id := operationId(request.IdempotencyKey)
	now := time.Now()
	operation := Operation{
		Provider:        ProviderSandbox,
		Id:              id,
		UserId:          request.UserId,
		IdempotencyKey:  request.IdempotencyKey,
		Direction:       DirectionDeposit,
		Method:          request.Method,
		Status:          StatusPending,
		Amount:          request.Amount,
		Fee:             s.Fee.For(request.Amount),
		ConfirmationURL: "https://sandbox.invalid/pay/" + id,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	s.remember(operation)
	return operation, nil
}

func (s *Sandbox) Payout(_ context.Context, request PayoutRequest) (Operation, error) {
	if err := request.Validate(); err != nil {
		return Operation{}, fmt.Errorf("%w: %w", ErrRejected, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unavailable {
		return Operation{}, ErrUnavailable
	}
	if existing, ok := s.byIdempotencyKey(request.IdempotencyKey); ok {
		return existing, nil
	}
	if request.Method == MethodCard {
		if _, ok := s.cards[request.CardToken]; !ok {
			return Operation{}, fmt.Errorf("%w: card token is unknown", ErrRejected)
		}
	}

	id := operationId(request.IdempotencyKey)
	now := time.Now()
	operation := Operation{
		Provider:       ProviderSandbox,
		Id:             id,
		UserId:         request.UserId,
		IdempotencyKey: request.IdempotencyKey,
		Direction:      DirectionPayout,
		Method:         request.Method,
		Status:         StatusPending,
		Amount:         request.Amount,
		Fee:            s.Fee.For(request.Amount),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	s.remember(operation)
	return operation, nil
}

func (s *Sandbox) Status(_ context.Context, id string) (Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unavailable {
		return Operation{}, ErrUnavailable
	}
	operation, ok := s.operations[id]
	if !ok {
		return Operation{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return operation, nil
}

// Advance переводит операцию в новое состояние и возвращает событие,
// которое провайдер прислал бы вебхуком.
func (s *Sandbox) Advance(id string, status Status, reason string) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	operation, ok := s.operations[id]
	if !ok {
		return Event{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if !operation.Status.CanFollow(status) {
		return Event{}, fmt.Errorf("%s cannot follow %s", status, operation.Status)
	}

	operation.Status = status
	operation.FailureReason = reason
	operation.UpdatedAt = time.Now()
	s.operations[id] = operation

	return Event{
		Id:            eventId(id, status),
		OperationId:   id,
		Provider:      ProviderSandbox,
		Status:        status,
		Amount:        operation.Amount,
		FailureReason: reason,
		OccurredAt:    operation.UpdatedAt,
	}, nil
}

func (s *Sandbox) Bind(_ context.Context, user uuid.UUID) (Binding, error) {
	if !s.CardsSupported {
		return Binding{}, ErrUnsupported
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unavailable {
		return Binding{}, ErrUnavailable
	}

	token := "sandbox-card-" + digest(user.String())[:16]
	// Данные карты пользователь вводит на стороне провайдера: система
	// не видит номера ни в запросе, ни в ответе.
	s.cards[token] = Card{
		Token:    token,
		Last4:    digits(token),
		Brand:    "MIR",
		ExpMonth: 12,
		ExpYear:  time.Now().Year() + 2,
		BoundAt:  time.Now(),
	}
	return Binding{Token: token, ConfirmationURL: "https://sandbox.invalid/bind/" + token}, nil
}

func (s *Sandbox) Card(_ context.Context, token string) (Card, error) {
	if !s.CardsSupported {
		return Card{}, ErrUnsupported
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	card, ok := s.cards[token]
	if !ok {
		return Card{}, fmt.Errorf("%w: card %s", ErrNotFound, token)
	}
	return card, nil
}

func (s *Sandbox) Unbind(_ context.Context, token string) error {
	if !s.CardsSupported {
		return ErrUnsupported
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.cards[token]; !ok {
		return fmt.Errorf("%w: card %s", ErrNotFound, token)
	}
	delete(s.cards, token)
	return nil
}

// byIdempotencyKey вызывается под мьютексом.
func (s *Sandbox) byIdempotencyKey(key string) (Operation, bool) {
	id, ok := s.byKey[key]
	if !ok {
		return Operation{}, false
	}
	operation, ok := s.operations[id]
	return operation, ok
}

// remember вызывается под мьютексом.
func (s *Sandbox) remember(operation Operation) {
	s.operations[operation.Id] = operation
	s.byKey[operation.IdempotencyKey] = operation.Id
}

func operationId(key string) string {
	return "sandbox-" + digest(key)[:16]
}

func eventId(operation string, status Status) string {
	return "sandbox-event-" + digest(operation + ":" + string(status))[:16]
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// digits берёт из токена четыре цифры для показа пользователю:
// настоящих последних цифр карты у песочницы нет.
func digits(token string) string {
	last := make([]byte, 0, 4)
	for i := len(token) - 1; i >= 0 && len(last) < 4; i-- {
		if token[i] >= '0' && token[i] <= '9' {
			last = append(last, token[i])
		}
	}
	for len(last) < 4 {
		last = append(last, '0')
	}
	return string(last)
}
