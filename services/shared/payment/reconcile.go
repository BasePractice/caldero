package payment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"wish/services"
)

// UnsettledSource перечисляет операции, застрявшие в незавершённом
// состоянии. Интерфейс объявлен здесь, у потребителя.
type UnsettledSource interface {
	Unsettled(ctx context.Context, olderThan time.Duration, limit int) ([]Operation, error)
}

// Reconciler сверяет наше состояние операций с состоянием у провайдера.
//
// Без сверки платёжный контур держится на одном допущении: вебхук всегда
// доходит. Он не доходит — сеть, перезапуск, ошибка обработки, — и операция
// остаётся в Pending навсегда, то есть деньги пользователя зависают
// без всякого признака сбоя.
type Reconciler struct {
	gateway Gateway
	store   OperationStore
	source  UnsettledSource

	// OlderThan — насколько операция должна залежаться, чтобы попасть
	// в сверку. Слишком малое значение превращает сверку в опрос
	// провайдера по каждой операции.
	OlderThan time.Duration
	// Limit ограничивает размер одного прохода: провайдер ограничивает
	// частоту запросов, и разобрать накопившееся за раз всё равно нельзя.
	Limit int
}

// Значения по умолчанию для сверки.
const (
	DefaultReconcileAge   = 15 * time.Minute
	DefaultReconcileLimit = 100
)

func NewReconciler(gateway Gateway, store OperationStore, source UnsettledSource) *Reconciler {
	return &Reconciler{
		gateway:   gateway,
		store:     store,
		source:    source,
		OlderThan: DefaultReconcileAge,
		Limit:     DefaultReconcileLimit,
	}
}

// Result — итог одного прохода сверки.
type Result struct {
	Checked int
	Updated int
	Failed  int
}

// Reconcile проходит по незавершённым операциям и подтягивает их состояние
// от провайдера. Ошибка по отдельной операции не прерывает проход: одна
// недоступная операция не повод оставить остальные незавершёнными.
func (r *Reconciler) Reconcile(ctx context.Context) (Result, error) {
	unsettled, err := r.source.Unsettled(ctx, r.OlderThan, r.Limit)
	if err != nil {
		return Result{}, fmt.Errorf("loading unsettled operations: %w", err)
	}

	var result Result
	for _, operation := range unsettled {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		result.Checked++

		remote, err := r.gateway.Status(ctx, operation.Id)
		if err != nil {
			result.Failed++
			slog.WarnContext(ctx, "Can't check operation at the provider",
				slog.String("operation", operation.Id), slog.String("err", err.Error()))
			// Недоступность провайдера означает, что и остальные вызовы
			// не пройдут: размыкатель цепи уже разомкнут или вот-вот
			// разомкнётся, и добивать его бессмысленно.
			if errors.Is(err, ErrUnavailable) || errors.Is(err, services.ErrCircuitOpen) {
				return result, err
			}
			continue
		}
		if remote.Status == operation.Status {
			continue
		}

		// Событие сверки помечено идентификатором, производным от операции
		// и статуса: повторная сверка того же расхождения не создаёт
		// второго применения.
		event := Event{
			Id:            fmt.Sprintf("reconcile:%s:%s", operation.Id, remote.Status),
			OperationId:   operation.Id,
			Provider:      remote.Provider,
			Status:        remote.Status,
			Amount:        remote.Amount,
			FailureReason: remote.FailureReason,
			OccurredAt:    remote.UpdatedAt,
		}
		if event.OccurredAt.IsZero() {
			event.OccurredAt = time.Now()
		}

		updated, err := r.store.Apply(ctx, event, event.ApplyTo)
		switch {
		case errors.Is(err, ErrEventIgnored):
			continue
		case err != nil:
			result.Failed++
			slog.ErrorContext(ctx, "Can't apply reconciled state",
				slog.String("operation", operation.Id), slog.String("err", err.Error()))
			continue
		}

		result.Updated++
		slog.InfoContext(ctx, "Operation state recovered by reconciliation",
			slog.String("operation", updated.Id),
			slog.String("status", string(updated.Status)))
	}
	return result, nil
}
