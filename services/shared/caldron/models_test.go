package caldron

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"wish/services/shared/credit"
)

func TestCanTransition(t *testing.T) {
	tests := []struct {
		name  string
		from  State
		to    State
		actor Actor
		want  error
	}{
		{"котёл становится готовым по факту взносов", StatePreparing, StateReady, ActorSystem, nil},
		{"создатель отменяет сбор", StatePreparing, StateCancelled, ActorCreator, nil},
		{"создатель отменяет готовый котёл", StateReady, StateCancelled, ActorCreator, nil},
		{"создатель завершает котёл", StateReady, StateSettled, ActorCreator, nil},
		{"готовый котёл возвращается к сбору", StateReady, StatePreparing, ActorSystem, nil},

		{"создатель не объявляет котёл готовым", StatePreparing, StateReady, ActorCreator, ErrForbiddenTransition},
		{"котёл не завершается до сбора", StatePreparing, StateSettled, ActorCreator, ErrInvalidTransition},
		{"завершённый котёл не отменяется", StateSettled, StateCancelled, ActorCreator, ErrInvalidTransition},
		{"отменённый котёл не оживает", StateCancelled, StatePreparing, ActorCreator, ErrInvalidTransition},
		{"неизвестное состояние", State("BOILING"), StateReady, ActorSystem, ErrInvalidTransition},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := CanTransition(test.from, test.to, test.actor)
			switch {
			case test.want == nil && err != nil:
				t.Errorf("переход отклонён: %v", err)
			case test.want != nil && !errors.Is(err, test.want):
				t.Errorf("получено %v, ожидалась %v", err, test.want)
			}
		})
	}
}

func TestTerminalStates(t *testing.T) {
	for _, state := range []State{StateSettled, StateCancelled} {
		t.Run(string(state), func(t *testing.T) {
			if !state.Terminal() {
				t.Errorf("состояние %s должно быть терминальным", state)
			}
			for _, to := range []State{StatePreparing, StateReady, StateSettled, StateCancelled} {
				for _, actor := range []Actor{ActorCreator, ActorSystem} {
					if err := CanTransition(state, to, actor); err == nil {
						t.Errorf("%s разрешил переход в %s для %s", state, to, actor)
					}
				}
			}
		})
	}
}

func TestCreateCaldronValidate(t *testing.T) {
	tests := []struct {
		name    string
		create  CreateCaldron
		wantErr bool
	}{
		{"точная сумма", CreateCaldron{Title: "Юбилей", Type: TypeGift,
			Mode: ModeFixed, Amount: 2_500_00}, false},
		{"индивидуальные суммы", CreateCaldron{Title: "Юбилей", Type: TypeLuck,
			Mode: ModeIndividual}, false},
		{"диапазон", CreateCaldron{Title: "Юбилей", Type: TypeGift,
			Mode: ModeRange, MinAmount: 1_000_00, MaxAmount: 5_000_00}, false},

		{"без названия", CreateCaldron{Type: TypeGift, Mode: ModeFixed, Amount: 100_00}, true},
		{"неизвестный тип", CreateCaldron{Title: "Юбилей", Type: "SOUP",
			Mode: ModeFixed, Amount: 100_00}, true},
		{"точная сумма без суммы", CreateCaldron{Title: "Юбилей", Type: TypeGift,
			Mode: ModeFixed}, true},
		{"точная сумма с диапазоном", CreateCaldron{Title: "Юбилей", Type: TypeGift,
			Mode: ModeFixed, Amount: 100_00, MinAmount: 50_00}, true},
		{"индивидуальный режим с суммой", CreateCaldron{Title: "Юбилей", Type: TypeGift,
			Mode: ModeIndividual, Amount: 100_00}, true},
		{"перевёрнутый диапазон", CreateCaldron{Title: "Юбилей", Type: TypeGift,
			Mode: ModeRange, MinAmount: 5_000_00, MaxAmount: 1_000_00}, true},
		{"взнос меньше рубля", CreateCaldron{Title: "Юбилей", Type: TypeGift,
			Mode: ModeFixed, Amount: 50}, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.create.Validate()
			if (err != nil) != test.wantErr {
				t.Errorf("Validate() = %v, ожидалась ошибка: %v", err, test.wantErr)
			}
		})
	}
}

func TestAddParticipantValidate(t *testing.T) {
	user := uuid.New()
	tests := []struct {
		name    string
		add     AddParticipant
		mode    ContributionMode
		wantErr bool
	}{
		{"фиксированный режим без суммы", AddParticipant{UserId: user}, ModeFixed, false},
		{"фиксированный режим с суммой", AddParticipant{UserId: user, Amount: 100_00}, ModeFixed, true},
		{"индивидуальный режим с суммой", AddParticipant{UserId: user, Amount: 100_00}, ModeIndividual, false},
		{"индивидуальный режим без суммы", AddParticipant{UserId: user}, ModeIndividual, true},
		{"диапазон без суммы", AddParticipant{UserId: user}, ModeRange, false},
		{"без пользователя", AddParticipant{}, ModeFixed, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.add.Validate(test.mode)
			if (err != nil) != test.wantErr {
				t.Errorf("Validate() = %v, ожидалась ошибка: %v", err, test.wantErr)
			}
		})
	}
}

func TestContributionFor(t *testing.T) {
	tests := []struct {
		name        string
		caldron     Caldron
		participant Participant
		requested   credit.Amount
		want        credit.Amount
		wantErr     bool
	}{
		{"точная сумма", Caldron{Mode: ModeFixed, Amount: 2_500_00},
			Participant{}, 0, 2_500_00, false},
		{"точная сумма подтверждена участником", Caldron{Mode: ModeFixed, Amount: 2_500_00},
			Participant{}, 2_500_00, 2_500_00, false},
		{"точная сумма не совпала", Caldron{Mode: ModeFixed, Amount: 2_500_00},
			Participant{}, 1_000_00, 0, true},
		{"индивидуальная сумма", Caldron{Mode: ModeIndividual},
			Participant{Expected: 3_000_00}, 0, 3_000_00, false},
		{"индивидуальная сумма не назначена", Caldron{Mode: ModeIndividual},
			Participant{}, 0, 0, true},
		{"диапазон", Caldron{Mode: ModeRange, MinAmount: 1_000_00, MaxAmount: 5_000_00},
			Participant{}, 2_000_00, 2_000_00, false},
		{"меньше нижней границы", Caldron{Mode: ModeRange, MinAmount: 1_000_00, MaxAmount: 5_000_00},
			Participant{}, 500_00, 0, true},
		{"больше верхней границы", Caldron{Mode: ModeRange, MinAmount: 1_000_00, MaxAmount: 5_000_00},
			Participant{}, 9_000_00, 0, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.caldron.ContributionFor(test.participant, test.requested)
			if (err != nil) != test.wantErr {
				t.Fatalf("ContributionFor() = %v, ожидалась ошибка: %v", err, test.wantErr)
			}
			if err == nil && got != test.want {
				t.Errorf("сумма взноса %s, ожидалась %s", got, test.want)
			}
		})
	}
}

// TestCompleteIgnoresArbiter фиксирует различие двух видов котла: арбитр
// организует сбор, но сам не скидывается, и ждать от него взнос нельзя.
func TestCompleteIgnoresArbiter(t *testing.T) {
	creator := uuid.New()
	member := uuid.New()

	arbiter := Caldron{
		CreatorId:           creator,
		CreatorParticipates: false,
		Participants: []Participant{
			{UserId: creator, State: ParticipantInvited},
			{UserId: member, State: ParticipantPaid},
		},
	}
	if !arbiter.Complete() {
		t.Error("котёл с арбитром не считается собранным, хотя участник внёс")
	}
	if len(arbiter.Members()) != 1 {
		t.Errorf("участников, от кого ждут взнос: %d, ожидался один", len(arbiter.Members()))
	}

	participating := arbiter
	participating.CreatorParticipates = true
	if participating.Complete() {
		t.Error("котёл собран, хотя создатель-участник не внёс")
	}

	empty := Caldron{CreatorId: creator, CreatorParticipates: true}
	if empty.Complete() {
		t.Error("пустой котёл считается собранным: разыгрывать было бы нечего")
	}
}
