package payment

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Memory — хранилище операций в памяти: локальный стенд и тесты.
//
// Боевое хранилище — таблица в базе, где Apply выполняется одной
// транзакцией с блокировкой строки операции. Здесь ту же роль играет
// мьютекс: важно не место хранения, а то, что чтение состояния,
// решение о переходе и запись неразделимы.
type Memory struct {
	mu         sync.Mutex
	operations map[string]Operation
	// applied — идентификаторы уже применённых событий. Провайдер
	// доставляет вебхук «хотя бы один раз», и без журнала повтор
	// провёл бы платёж дважды.
	applied map[string]struct{}
}

func NewMemory() *Memory {
	return &Memory{
		operations: make(map[string]Operation),
		applied:    make(map[string]struct{}),
	}
}

// Put сохраняет операцию, созданную у провайдера.
func (m *Memory) Put(_ context.Context, operation Operation) error {
	if operation.Id == "" {
		return fmt.Errorf("operation id is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.operations[operation.Id] = operation
	return nil
}

// Operation возвращает известное состояние операции.
func (m *Memory) Operation(_ context.Context, id string) (Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	operation, ok := m.operations[id]
	if !ok {
		return Operation{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return operation, nil
}

// Apply применяет событие к операции.
func (m *Memory) Apply(
	_ context.Context,
	event Event,
	transition func(Operation) (Operation, error),
) (Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	operation, ok := m.operations[event.OperationId]
	if !ok {
		return Operation{}, fmt.Errorf("%w: %s", ErrNotFound, event.OperationId)
	}
	if _, seen := m.applied[event.Id]; seen {
		return operation, fmt.Errorf("%w: event %s is already applied", ErrEventIgnored, event.Id)
	}

	updated, err := transition(operation)
	if err != nil {
		// Отметка о событии не ставится: событие не применено, и повторная
		// доставка должна пройти тот же путь.
		return operation, err
	}

	m.operations[updated.Id] = updated
	m.applied[event.Id] = struct{}{}
	return updated, nil
}

// Unsettled возвращает операции, застрявшие в незавершённом состоянии
// дольше указанного срока. По ним идёт сверка: вебхук может не дойти.
func (m *Memory) Unsettled(_ context.Context, olderThan time.Duration, limit int) ([]Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	deadline := time.Now().Add(-olderThan)
	unsettled := make([]Operation, 0, len(m.operations))
	for _, operation := range m.operations {
		if operation.Status.Terminal() || operation.UpdatedAt.After(deadline) {
			continue
		}
		unsettled = append(unsettled, operation)
		if limit > 0 && len(unsettled) >= limit {
			break
		}
	}
	return unsettled, nil
}
