package payment

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"wish/services"
)

// Resilient оборачивает провайдера размыкателем цепи.
//
// Кэшировать здесь нечего: состояние операции меняется, и отдавать
// его из кэша — значит проводить деньги по устаревшим данным.
//
// Размыкатель не отменяет идемпотентности, а требует её: отказ вызова
// не означает, что операции у провайдера нет. Повтор с тем же ключом
// обязан вернуть ту же операцию, иначе размыкание цепи превращается
// в двойные платежи.
type Resilient struct {
	gateway Gateway
	breaker *services.Breaker
}

// Параметры размыкателя: пять отказов подряд размыкают цепь на полминуты.
const (
	breakerThreshold  = 5
	breakerResetAfter = 30 * time.Second
)

func NewResilient(gateway Gateway) *Resilient {
	return &Resilient{
		gateway: gateway,
		// Размыкается только на недоступность провайдера: отказ в платеже —
		// это его нормальный ответ, и цепь от него размыкаться не должна.
		breaker: services.NewBreaker(
			"payment-"+string(gateway.Provider()), breakerThreshold, breakerResetAfter,
			func(err error) bool { return errors.Is(err, ErrUnavailable) },
		),
	}
}

func (r *Resilient) Provider() Provider {
	return r.gateway.Provider()
}

func (r *Resilient) Deposit(ctx context.Context, request DepositRequest) (Operation, error) {
	return r.do(ctx, func(ctx context.Context) (Operation, error) {
		return r.gateway.Deposit(ctx, request)
	})
}

func (r *Resilient) Payout(ctx context.Context, request PayoutRequest) (Operation, error) {
	return r.do(ctx, func(ctx context.Context) (Operation, error) {
		return r.gateway.Payout(ctx, request)
	})
}

func (r *Resilient) Status(ctx context.Context, id string) (Operation, error) {
	return r.do(ctx, func(ctx context.Context) (Operation, error) {
		return r.gateway.Status(ctx, id)
	})
}

// Bind, Card и Unbind доступны, только если провайдер поддерживает
// привязку карт; иначе возвращается ErrUnsupported.
func (r *Resilient) Bind(ctx context.Context, user uuid.UUID) (Binding, error) {
	vault, ok := r.gateway.(CardVault)
	if !ok {
		return Binding{}, ErrUnsupported
	}
	var binding Binding
	err := r.breaker.Do(ctx, func(ctx context.Context) error {
		bound, err := vault.Bind(ctx, user)
		if err != nil {
			return err
		}
		binding = bound
		return nil
	})
	return binding, err
}

func (r *Resilient) Card(ctx context.Context, token string) (Card, error) {
	vault, ok := r.gateway.(CardVault)
	if !ok {
		return Card{}, ErrUnsupported
	}
	var card Card
	err := r.breaker.Do(ctx, func(ctx context.Context) error {
		found, err := vault.Card(ctx, token)
		if err != nil {
			return err
		}
		card = found
		return nil
	})
	return card, err
}

func (r *Resilient) Unbind(ctx context.Context, token string) error {
	vault, ok := r.gateway.(CardVault)
	if !ok {
		return ErrUnsupported
	}
	return r.breaker.Do(ctx, func(ctx context.Context) error {
		return vault.Unbind(ctx, token)
	})
}

func (r *Resilient) do(
	ctx context.Context,
	call func(context.Context) (Operation, error),
) (Operation, error) {
	var operation Operation
	err := r.breaker.Do(ctx, func(ctx context.Context) error {
		result, err := call(ctx)
		if err != nil {
			return err
		}
		operation = result
		return nil
	})
	return operation, err
}
