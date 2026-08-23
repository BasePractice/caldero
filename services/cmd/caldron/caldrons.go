package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"wish/services/shared/caldron"
	"wish/services/shared/credit"
	"wish/services/shared/notify"
	"wish/services/shared/wallets"

	"github.com/google/uuid"
)

// Ошибки операций. Отделены от сбоев БД: каждая — конкретный ответ клиенту.
var (
	// ErrForbidden — операция доступна не этому участнику.
	ErrForbidden = errors.New("operation is not allowed for this user")
	// ErrWalletUnavailable — кошелёк недоступен, средства не двигались.
	ErrWalletUnavailable = errors.New("wallet is unavailable")
	// ErrNotReady — котёл ещё не собран.
	ErrNotReady = errors.New("caldron is not ready")
)

// Wallet — то, что нужно от сервиса кошелька. Интерфейс объявлен здесь,
// у потребителя; реализация — общий клиент в services/shared/wallets.
type Wallet interface {
	Wallet(ctx context.Context, owner uuid.UUID) (wallets.Info, error)
	Transfer(ctx context.Context, owner uuid.UUID, params wallets.TransferParams) error
}

// Caldrons — операции над котлом вместе с их денежными последствиями.
//
// Средства котла лежат на его собственном кошельке. Владельцем кошелька
// выступает сам котёл: его идентификатор — такой же UUID, как у человека,
// и сервису кошелька безразлично, кто владелец. Держать сбор на кошельке
// создателя нельзя — он смешался бы с его собственными деньгами, а вместе
// с ними попал бы под любое списание создателя.
type Caldrons struct {
	db       Database
	wallet   Wallet
	notifier *notify.Client
}

func NewCaldrons(db Database, wallet Wallet, notifier *notify.Client) *Caldrons {
	return &Caldrons{db: db, wallet: wallet, notifier: notifier}
}

func (c *Caldrons) Create(
	ctx context.Context,
	creator uuid.UUID,
	create caldron.CreateCaldron,
) (caldron.Caldron, error) {
	pot, err := c.db.Create(ctx, caldron.Caldron{
		CreatorId:           creator,
		Title:               create.Title,
		Type:                create.Type,
		CreatorParticipates: create.CreatorParticipates,
		Mode:                create.Mode,
		Amount:              create.Amount,
		MinAmount:           create.MinAmount,
		MaxAmount:           create.MaxAmount,
	})
	if err != nil {
		return caldron.Caldron{}, err
	}

	if !create.CreatorParticipates {
		return pot, nil
	}
	// Создатель-участник скидывается наравне со всеми, поэтому попадает
	// в список сразу: отдельная кнопка «добавить себя» ничего не даёт.
	add := caldron.AddParticipant{UserId: creator}
	if pot.Mode == caldron.ModeIndividual {
		// В индивидуальном режиме сумму себе создатель назначает сам,
		// как и остальным.
		return pot, nil
	}
	updated, err := c.db.AddParticipant(ctx, pot.Id, add)
	if err != nil {
		return caldron.Caldron{}, err
	}
	return updated, nil
}

// Caldron отдаёт котёл тому, кто в нём участвует. Посторонним котёл
// не показывается вовсе: чужой сбор — не публичные данные.
func (c *Caldrons) Caldron(ctx context.Context, viewer, id uuid.UUID) (caldron.Caldron, error) {
	pot, err := c.db.Caldron(ctx, id)
	if err != nil {
		return caldron.Caldron{}, err
	}
	if pot.CreatorId != viewer && !pot.IsParticipant(viewer) {
		return caldron.Caldron{}, ErrNotFound
	}
	return pot, nil
}

func (c *Caldrons) List(ctx context.Context, user uuid.UUID) ([]caldron.Caldron, error) {
	return c.db.ByUser(ctx, user)
}

// AddParticipant добавляет участника. Право только у создателя и только
// в состоянии подготовки — по README.
func (c *Caldrons) AddParticipant(
	ctx context.Context,
	creator, id uuid.UUID,
	add caldron.AddParticipant,
) (caldron.Caldron, error) {
	pot, err := c.owned(ctx, creator, id)
	if err != nil {
		return caldron.Caldron{}, err
	}
	if err = add.Validate(pot.Mode); err != nil {
		return caldron.Caldron{}, err
	}

	updated, err := c.db.AddParticipant(ctx, id, add)
	if err != nil {
		return caldron.Caldron{}, err
	}

	c.publish(ctx, add.UserId, notify.EventCaldronMemberAdded, map[string]string{
		"caldron": updated.Title,
		"amount":  expectedAmountText(updated, add.Amount),
	}, fmt.Sprintf("caldron:%s:added:%s", id, add.UserId))
	return updated, nil
}

func (c *Caldrons) RemoveParticipant(
	ctx context.Context,
	creator, id, user uuid.UUID,
) (caldron.Caldron, error) {
	if _, err := c.owned(ctx, creator, id); err != nil {
		return caldron.Caldron{}, err
	}
	return c.db.RemoveParticipant(ctx, id, user)
}

// Contribute вносит средства участника в котёл.
//
// Проверка суммы идёт против кошелька, а не на слово: перевод либо
// проходит, либо не проходит, и «внёс» отмечается только после него.
func (c *Caldrons) Contribute(
	ctx context.Context,
	user, id uuid.UUID,
	requested credit.Amount,
) (caldron.Caldron, error) {
	pot, amount, err := c.db.StartContribution(ctx, id, user, requested)
	if err != nil {
		return caldron.Caldron{}, err
	}
	if c.wallet == nil {
		return caldron.Caldron{}, fmt.Errorf("%w: wallet service is not configured", ErrWalletUnavailable)
	}

	target, err := c.walletOf(ctx, pot)
	if err != nil {
		return caldron.Caldron{}, err
	}
	source, err := c.wallet.Wallet(ctx, user)
	if err != nil {
		return caldron.Caldron{}, fmt.Errorf("%w: %s", ErrWalletUnavailable, err)
	}

	// Ключ идемпотентности выведен из котла и участника: повтор запроса
	// после обрыва связи не спишет взнос дважды.
	if err = c.wallet.Transfer(ctx, user, wallets.TransferParams{
		IdempotencyKey: fmt.Sprintf("caldron:%s:contribution:%s", id, user),
		Source:         source.Id,
		Target:         target,
		Value:          amount,
		Message:        "Взнос в котёл " + pot.Title,
	}); err != nil {
		return caldron.Caldron{}, fmt.Errorf("%w: %s", ErrWalletUnavailable, err)
	}

	updated, err := c.db.MarkPaid(ctx, id, user, amount)
	if err != nil {
		// Средства уже в котле, а отметка не проставлена. Повтор пройдёт
		// тем же путём: перевод с тем же ключом не повторится, а отметка
		// встанет.
		return caldron.Caldron{}, err
	}

	if updated.State == caldron.StateReady && pot.State != caldron.StateReady {
		c.announce(ctx, updated, "готов к розыгрышу")
	}
	return updated, nil
}

// Cancel отменяет котёл и возвращает средства всем внёсшим.
func (c *Caldrons) Cancel(ctx context.Context, creator, id uuid.UUID) (caldron.Caldron, error) {
	if _, err := c.owned(ctx, creator, id); err != nil {
		return caldron.Caldron{}, err
	}

	// Сначала состояние, потом деньги: отменённый котёл перестаёт
	// принимать взносы, и возвращать приходится фиксированный набор.
	cancelled, err := c.db.Transition(ctx, id, caldron.StateCancelled, caldron.ActorCreator)
	if err != nil {
		return caldron.Caldron{}, err
	}

	if err = c.refund(ctx, cancelled); err != nil {
		// Возврат не завершён: котёл останется в очереди фоновой задачи,
		// и средства вернутся сами. Отмену это не отменяет.
		slog.ErrorContext(ctx, "Refund is not complete, will retry in background",
			slog.String("caldron", id.String()), slog.String("err", err.Error()))
	}

	c.announce(ctx, cancelled, "отменён, средства возвращены")
	return c.db.Caldron(ctx, id)
}

// Settle передаёт средства котла получателю и завершает его.
//
// Кто именно получатель, решает не этот сервис: котёл подарков и котёл
// удачи выбирают победителя по-своему. Здесь только передача средств.
func (c *Caldrons) Settle(
	ctx context.Context,
	creator, id, winner uuid.UUID,
) (caldron.Caldron, error) {
	pot, err := c.owned(ctx, creator, id)
	if err != nil {
		return caldron.Caldron{}, err
	}
	if pot.State != caldron.StateReady {
		return caldron.Caldron{}, fmt.Errorf("%w: state is %s", ErrNotReady, pot.State)
	}
	if !pot.IsParticipant(winner) {
		return caldron.Caldron{}, fmt.Errorf("%w: winner is not a participant", ErrForbidden)
	}
	if pot.Collected <= 0 {
		return caldron.Caldron{}, fmt.Errorf("%w: nothing collected", ErrNotReady)
	}
	if c.wallet == nil {
		return caldron.Caldron{}, fmt.Errorf("%w: wallet service is not configured", ErrWalletUnavailable)
	}

	source, err := c.walletOf(ctx, pot)
	if err != nil {
		return caldron.Caldron{}, err
	}
	target, err := c.wallet.Wallet(ctx, winner)
	if err != nil {
		return caldron.Caldron{}, fmt.Errorf("%w: %s", ErrWalletUnavailable, err)
	}

	// Перевод раньше смены состояния: объявить котёл завершённым, не отдав
	// средства, хуже, чем повторить перевод — повтор отсекается ключом.
	if err = c.wallet.Transfer(ctx, pot.Id, wallets.TransferParams{
		IdempotencyKey: fmt.Sprintf("caldron:%s:payout", id),
		Source:         source,
		Target:         target.Id,
		Value:          pot.Collected,
		Message:        "Выигрыш в котле " + pot.Title,
	}); err != nil {
		return caldron.Caldron{}, fmt.Errorf("%w: %s", ErrWalletUnavailable, err)
	}

	settled, err := c.db.Transition(ctx, id, caldron.StateSettled, caldron.ActorCreator)
	if err != nil {
		return caldron.Caldron{}, err
	}

	c.announce(ctx, settled, "завершён, средства переданы победителю")
	return settled, nil
}

// RefundPending добивает незавершённые возвраты. Без этого сбой посреди
// отмены оставил бы средства участников в котле навсегда.
func (c *Caldrons) RefundPending(ctx context.Context) error {
	if c.wallet == nil {
		return nil
	}

	const limit = 50
	pending, err := c.db.PendingRefunds(ctx, limit)
	if err != nil {
		return err
	}
	for _, pot := range pending {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err = c.refund(ctx, pot); err != nil {
			slog.WarnContext(ctx, "Can't finish refund",
				slog.String("caldron", pot.Id.String()), slog.String("err", err.Error()))
		}
	}
	return nil
}

// refund возвращает средства всем, кто их внёс.
//
// Возврат идемпотентен за счёт ключа: повтор при сбое не вернёт дважды.
// Отметка ставится после перевода, поэтому в худшем случае повтор
// обратится к кошельку с тем же ключом и получит ту же транзакцию.
func (c *Caldrons) refund(ctx context.Context, pot caldron.Caldron) error {
	if c.wallet == nil {
		return fmt.Errorf("%w: wallet service is not configured", ErrWalletUnavailable)
	}
	if pot.WalletId == nil {
		// Кошелька нет — значит, и взносов не было.
		return nil
	}

	var failed error
	for _, participant := range pot.Participants {
		if participant.State != caldron.ParticipantPaid {
			continue
		}

		target, err := c.wallet.Wallet(ctx, participant.UserId)
		if err != nil {
			failed = errors.Join(failed, fmt.Errorf("wallet of %s: %w", participant.UserId, err))
			continue
		}
		if err = c.wallet.Transfer(ctx, pot.Id, wallets.TransferParams{
			IdempotencyKey: fmt.Sprintf("caldron:%s:refund:%s", pot.Id, participant.UserId),
			Source:         *pot.WalletId,
			Target:         target.Id,
			Value:          participant.Contributed,
			Message:        "Возврат взноса из котла " + pot.Title,
		}); err != nil {
			failed = errors.Join(failed, fmt.Errorf("refund to %s: %w", participant.UserId, err))
			continue
		}
		if err = c.db.MarkRefunded(ctx, pot.Id, participant.UserId); err != nil {
			failed = errors.Join(failed, err)
		}
	}
	return failed
}

// walletOf возвращает кошелёк котла, заводя его при первом взносе.
func (c *Caldrons) walletOf(ctx context.Context, pot caldron.Caldron) (uuid.UUID, error) {
	if pot.WalletId != nil {
		return *pot.WalletId, nil
	}

	info, err := c.wallet.Wallet(ctx, pot.Id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: %s", ErrWalletUnavailable, err)
	}
	if err = c.db.SetWallet(ctx, pot.Id, info.Id); err != nil {
		return uuid.Nil, err
	}
	return info.Id, nil
}

func (c *Caldrons) owned(ctx context.Context, creator, id uuid.UUID) (caldron.Caldron, error) {
	pot, err := c.db.Caldron(ctx, id)
	if err != nil {
		return caldron.Caldron{}, err
	}
	if pot.CreatorId != creator {
		// Чужой котёл отдаётся как несуществующий: подтверждать его
		// наличие постороннему незачем.
		if !pot.IsParticipant(creator) {
			return caldron.Caldron{}, ErrNotFound
		}
		return caldron.Caldron{}, fmt.Errorf("%w: only the creator can do this", ErrForbidden)
	}
	return pot, nil
}

// announce оповещает всех участников о смене состояния котла.
func (c *Caldrons) announce(ctx context.Context, pot caldron.Caldron, state string) {
	for _, participant := range pot.Participants {
		c.publish(ctx, participant.UserId, notify.EventCaldronStateChanged, map[string]string{
			"caldron": pot.Title,
			"state":   state,
		}, fmt.Sprintf("caldron:%s:state:%s:%s", pot.Id, pot.State, participant.UserId))
	}
}

// publish отправляет оповещение. Сбой оповещения не отменяет операцию:
// взнос остаётся внесённым, даже если сообщение не ушло.
func (c *Caldrons) publish(
	ctx context.Context,
	user uuid.UUID,
	eventType notify.EventType,
	payload map[string]string,
	dedupKey string,
) {
	if !c.notifier.Enabled() {
		return
	}
	if err := c.notifier.Publish(ctx, notify.PublishEvent{
		UserId:   user,
		Type:     eventType,
		Payload:  payload,
		DedupKey: dedupKey,
	}); err != nil {
		slog.ErrorContext(ctx, "Can't publish notification",
			slog.String("event", string(eventType)), slog.String("err", err.Error()))
	}
}

// expectedAmountText объясняет участнику, сколько с него ждут. У диапазона
// точной суммы нет, и подставлять ноль было бы враньём.
func expectedAmountText(pot caldron.Caldron, individual credit.Amount) string {
	switch pot.Mode {
	case caldron.ModeFixed:
		return pot.Amount.String()
	case caldron.ModeIndividual:
		return individual.String()
	case caldron.ModeRange:
		return fmt.Sprintf("от %s до %s", pot.MinAmount, pot.MaxAmount)
	default:
		return ""
	}
}
