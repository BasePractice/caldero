// Package caldron описывает котёл: состояния, участников и правила взносов.
// Пакет общий: котёл подарков (T-054) и котёл удачи (T-055) используют
// ту же модель, добавляя к ней только свою часть розыгрыша.
package caldron

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"wish/services/shared/credit"
)

// Type — вид котла. Заложен с самого начала, чтобы розыгрыш подарков
// и розыгрыш денег не переписывали общую модель.
type Type string

const (
	// TypeGift — котёл подарков: победителю достаётся набор подарков
	// из его списка (T-054).
	TypeGift Type = "GIFT"
	// TypeLuck — котёл удачи: победителю достаётся вся сумма (T-055).
	TypeLuck Type = "LUCK"
)

func (t Type) Valid() bool {
	return t == TypeGift || t == TypeLuck
}

// State — состояние котла. Названия из README: подготовка, готов,
// плюс завершён и отменён.
type State string

const (
	// StatePreparing — идёт сбор: участников добавляют, средства вносят.
	StatePreparing State = "PREPARING"
	// StateReady — все участники внесли средства, можно разыгрывать.
	StateReady State = "READY"
	// StateSettled — средства переданы получателю. Терминальное.
	StateSettled State = "SETTLED"
	// StateCancelled — котёл отменён, средства возвращены. Терминальное.
	StateCancelled State = "CANCELLED"
)

func (s State) Terminal() bool {
	return s == StateSettled || s == StateCancelled
}

func (s State) Valid() bool {
	switch s {
	case StatePreparing, StateReady, StateSettled, StateCancelled:
		return true
	default:
		return false
	}
}

// Actor — кто выполняет переход.
type Actor string

const (
	// ActorCreator — создатель котла: он же арбитр, если не участвует.
	ActorCreator Actor = "CREATOR"
	// ActorSystem — сам сервис: перевод в «готов» происходит по факту
	// последнего взноса, а не по чьей-то команде.
	ActorSystem Actor = "SYSTEM"
)

// Ошибки переходов.
var (
	ErrInvalidTransition   = errors.New("invalid caldron state transition")
	ErrForbiddenTransition = errors.New("transition is not allowed for this actor")
)

// transitions — таблица допустимых переходов. Явная таблица, а не набор
// условий по коду: у котла четыре состояния и два участника перехода,
// и разложенные по обработчикам правила разъезжаются на первой же правке.
var transitions = map[State]map[State][]Actor{
	StatePreparing: {
		// В «готов» котёл переводит не человек, а факт: внесли все.
		StateReady:     {ActorSystem},
		StateCancelled: {ActorCreator},
	},
	StateReady: {
		StateSettled:   {ActorCreator, ActorSystem},
		StateCancelled: {ActorCreator},
		// Участник может выйти или добавиться новый — тогда котёл
		// возвращается к сбору.
		StatePreparing: {ActorSystem},
	},
}

// CanTransition проверяет переход и возвращает причину отказа.
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

// ContributionMode — как определяется сумма взноса. Все три способа
// из README: точная сумма, индивидуальная и диапазон.
type ContributionMode string

const (
	// ModeFixed — одна и та же сумма для всех.
	ModeFixed ContributionMode = "FIXED"
	// ModeIndividual — сумму каждому участнику назначает создатель.
	ModeIndividual ContributionMode = "INDIVIDUAL"
	// ModeRange — участник вносит сколько хочет в пределах диапазона.
	ModeRange ContributionMode = "RANGE"
)

func (m ContributionMode) Valid() bool {
	switch m {
	case ModeFixed, ModeIndividual, ModeRange:
		return true
	default:
		return false
	}
}

// ParticipantState — состояние участника.
type ParticipantState string

const (
	// ParticipantInvited — добавлен, но ещё не внёс средства.
	ParticipantInvited ParticipantState = "INVITED"
	// ParticipantPaid — средства внесены и лежат на кошельке котла.
	ParticipantPaid ParticipantState = "PAID"
	// ParticipantRefunded — средства возвращены при отмене.
	ParticipantRefunded ParticipantState = "REFUNDED"
)

// MaxParticipants ограничивает размер котла: список участников приходит
// от пользователя, и без предела один котёл упирается в память сервиса.
const MaxParticipants = 100

// MinContribution — нижняя граница взноса: рубль.
const MinContribution = credit.Amount(100)

// Caldron — котёл.
type Caldron struct {
	Id        uuid.UUID `json:"id"`
	CreatorId uuid.UUID `json:"creator_id"`
	Title     string    `json:"title"`
	Type      Type      `json:"type"`
	State     State     `json:"state"`
	// CreatorParticipates различает два вида котла из README: создатель
	// либо скидывается вместе со всеми, либо остаётся арбитром.
	CreatorParticipates bool             `json:"creator_participates"`
	Mode                ContributionMode `json:"mode"`
	// Amount задан у режима FIXED, Min и Max — у RANGE.
	Amount    credit.Amount `json:"amount,omitempty"`
	MinAmount credit.Amount `json:"min_amount,omitempty"`
	MaxAmount credit.Amount `json:"max_amount,omitempty"`
	// ArbiterId — участник, которому создатель поручил запустить розыгрыш.
	// Пустое значение означает, что запускает сам создатель.
	ArbiterId *uuid.UUID `json:"arbiter_id,omitempty"`
	// Commitment — обязательство розыгрыша: хеш зерна, опубликованный
	// заранее. Само зерно раскрывается только после розыгрыша.
	Commitment string `json:"commitment,omitempty"`
	// WalletId — кошелёк котла. Средства участников лежат на нём,
	// а не на кошельке создателя: иначе сбор смешивается с его деньгами.
	WalletId *uuid.UUID `json:"wallet_id,omitempty"`
	// Collected — сколько уже внесено.
	Collected   credit.Amount `json:"collected"`
	SettledAt   *time.Time    `json:"settled_at,omitempty"`
	CancelledAt *time.Time    `json:"cancelled_at,omitempty"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`

	// Participants заполняется при чтении карточки котла.
	Participants []Participant `json:"participants,omitempty"`
}

// Participant — участник котла.
type Participant struct {
	CaldronId uuid.UUID `json:"caldron_id"`
	UserId    uuid.UUID `json:"user_id"`
	// Expected — сколько участник должен внести. У FIXED берётся из котла,
	// у INDIVIDUAL назначается создателем, у RANGE не задан заранее.
	Expected    credit.Amount    `json:"expected,omitempty"`
	Contributed credit.Amount    `json:"contributed"`
	State       ParticipantState `json:"state"`
	CreatedAt   time.Time        `json:"created_at"`
	UpdatedAt   time.Time        `json:"updated_at"`
}

// MaxTitle ограничивает название котла.
const MaxTitle = 200

// CreateCaldron — запрос на создание котла.
type CreateCaldron struct {
	Title               string           `json:"title"`
	Type                Type             `json:"type"`
	Mode                ContributionMode `json:"mode"`
	CreatorParticipates bool             `json:"creator_participates"`
	Amount              credit.Amount    `json:"amount,omitempty"`
	MinAmount           credit.Amount    `json:"min_amount,omitempty"`
	MaxAmount           credit.Amount    `json:"max_amount,omitempty"`
}

// Validate возвращает причину отказа, а не просто false.
func (c CreateCaldron) Validate() error {
	if c.Title == "" {
		return errors.New("title is required")
	}
	if len([]rune(c.Title)) > MaxTitle {
		return fmt.Errorf("title must not exceed %d characters", MaxTitle)
	}
	if !c.Type.Valid() {
		return fmt.Errorf("type must be one of %s, %s", TypeGift, TypeLuck)
	}
	if !c.Mode.Valid() {
		return fmt.Errorf("mode must be one of %s, %s, %s", ModeFixed, ModeIndividual, ModeRange)
	}

	switch c.Mode {
	case ModeFixed:
		if c.Amount < MinContribution {
			return fmt.Errorf("amount must be at least %s", MinContribution)
		}
		if c.MinAmount != 0 || c.MaxAmount != 0 {
			return fmt.Errorf("range bounds must be empty for %s mode", ModeFixed)
		}
	case ModeIndividual:
		if c.Amount != 0 || c.MinAmount != 0 || c.MaxAmount != 0 {
			return fmt.Errorf("amounts must be empty for %s mode: they are set per participant", ModeIndividual)
		}
	case ModeRange:
		if c.MinAmount < MinContribution {
			return fmt.Errorf("min_amount must be at least %s", MinContribution)
		}
		if c.MaxAmount < c.MinAmount {
			return fmt.Errorf("max_amount %s is less than min_amount %s", c.MaxAmount, c.MinAmount)
		}
		if c.Amount != 0 {
			return fmt.Errorf("amount must be empty for %s mode", ModeRange)
		}
	}
	return nil
}

func (c CreateCaldron) String() string {
	return fmt.Sprintf("{type=%s, mode=%s}", c.Type, c.Mode)
}

// AddParticipant — запрос на добавление участника.
type AddParticipant struct {
	UserId uuid.UUID `json:"user_id"`
	// Amount обязателен в режиме INDIVIDUAL и запрещён в остальных.
	Amount credit.Amount `json:"amount,omitempty"`
}

// ErrInvalidParticipant — заявка на участие не проходит проверку.
//
// Отдельная ошибка нужна потому, что проверка требует режима котла
// и потому выполняется в сервисе, а не в обработчике. Без доменной
// ошибки её причина неотличима от сбоя базы: обработчик отвечал 500
// и писал в журнал ERROR на обычную опечатку в запросе.
var ErrInvalidParticipant = errors.New("invalid participant request")

// Validate проверяет запрос в контексте котла: без него нельзя понять,
// нужна ли сумма.
func (a AddParticipant) Validate(mode ContributionMode) error {
	if a.UserId == uuid.Nil {
		return fmt.Errorf("%w: user_id is required", ErrInvalidParticipant)
	}
	if mode == ModeIndividual {
		if a.Amount < MinContribution {
			return fmt.Errorf("%w: amount must be at least %s in %s mode",
				ErrInvalidParticipant, MinContribution, ModeIndividual)
		}
		return nil
	}
	if a.Amount != 0 {
		return fmt.Errorf("%w: amount is set by the caldron in %s mode",
			ErrInvalidParticipant, mode)
	}
	return nil
}

// ErrInvalidContribution — сумма взноса не подходит под правила котла.
var ErrInvalidContribution = errors.New("invalid contribution amount")

// ContributionFor определяет, сколько участник должен внести.
//
// Правило зависит от режима, и решается это здесь, а не в обработчике:
// иначе проверка суммы разъедется с тем, что показано пользователю.
func (c Caldron) ContributionFor(participant Participant, requested credit.Amount) (credit.Amount, error) {
	switch c.Mode {
	case ModeFixed:
		if requested != 0 && requested != c.Amount {
			return 0, fmt.Errorf("%w: caldron expects exactly %s", ErrInvalidContribution, c.Amount)
		}
		return c.Amount, nil
	case ModeIndividual:
		if participant.Expected < MinContribution {
			return 0, fmt.Errorf("%w: participant has no assigned amount", ErrInvalidContribution)
		}
		if requested != 0 && requested != participant.Expected {
			return 0, fmt.Errorf("%w: participant is expected to contribute %s",
				ErrInvalidContribution, participant.Expected)
		}
		return participant.Expected, nil
	case ModeRange:
		if requested < c.MinAmount || requested > c.MaxAmount {
			return 0, fmt.Errorf("%w: amount must be between %s and %s",
				ErrInvalidContribution, c.MinAmount, c.MaxAmount)
		}
		return requested, nil
	default:
		return 0, fmt.Errorf("%w: unknown mode %s", ErrInvalidContribution, c.Mode)
	}
}

// Members возвращает всех, от кого ждут взнос. Создатель-арбитр в это
// число не входит: он организует сбор, а не участвует в нём.
func (c Caldron) Members() []Participant {
	members := make([]Participant, 0, len(c.Participants))
	for _, participant := range c.Participants {
		if participant.UserId == c.CreatorId && !c.CreatorParticipates {
			continue
		}
		members = append(members, participant)
	}
	return members
}

// Complete сообщает, что внесли все, от кого этого ждали. Котёл без
// участников готовым не считается: разыгрывать было бы нечего.
func (c Caldron) Complete() bool {
	members := c.Members()
	if len(members) == 0 {
		return false
	}
	for _, participant := range members {
		if participant.State != ParticipantPaid {
			return false
		}
	}
	return true
}

// CanDraw сообщает, вправе ли пользователь запустить розыгрыш.
//
// По README это создатель или назначенный им арбитр из числа участников.
// Право не зависит от того, участвует ли создатель в сборе: организатор
// он в любом случае.
func (c Caldron) CanDraw(user uuid.UUID) bool {
	if c.CreatorId == user {
		return true
	}
	return c.ArbiterId != nil && *c.ArbiterId == user
}

// ExpectedTotal — на какую сумму котёл рассчитан.
//
// Нужна до того, как все внесли: список подарков проверяется по ней ещё
// на этапе сбора. Для внёсших берётся фактический взнос, для остальных —
// то, чего от них ждут; у диапазона это нижняя граница, потому что
// рассчитывать на верхнюю значит обещать участнику больше, чем он получит.
func (c Caldron) ExpectedTotal() credit.Amount {
	var total credit.Amount
	for _, participant := range c.Members() {
		switch {
		case participant.State == ParticipantPaid:
			total += participant.Contributed
		case c.Mode == ModeRange:
			total += c.MinAmount
		default:
			total += participant.Expected
		}
	}
	return total
}

// IsParticipant сообщает, входит ли пользователь в котёл.
func (c Caldron) IsParticipant(user uuid.UUID) bool {
	for _, participant := range c.Participants {
		if participant.UserId == user {
			return true
		}
	}
	return false
}
