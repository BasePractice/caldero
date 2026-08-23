package wishlist

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"wish/services/shared/marketplace"
)

func TestCanTransition(t *testing.T) {
	tests := []struct {
		name  string
		from  State
		to    State
		actor Actor
		want  error
	}{
		{"владелец скрывает элемент", StateVisible, StateHidden, ActorOwner, nil},
		{"владелец возвращает элемент в список", StateHidden, StateVisible, ActorOwner, nil},
		{"даритель резервирует", StateVisible, StateChosen, ActorGiver, nil},
		{"даритель снимает резерв", StateChosen, StateVisible, ActorGiver, nil},
		{"система освобождает просроченный резерв", StateChosen, StateVisible, ActorSystem, nil},
		{"одаряемый подтверждает", StateChosen, StateConfirmed, ActorOwner, nil},
		{"одаряемый отклоняет", StateChosen, StateRejected, ActorOwner, nil},
		{"даритель акцептует", StateConfirmed, StateAccepted, ActorGiver, nil},
		{"даритель отказывается после подтверждения", StateConfirmed, StateVisible, ActorGiver, nil},

		{"даритель не резервирует скрытое", StateHidden, StateChosen, ActorGiver, ErrInvalidTransition},
		{"даритель не подтверждает за одаряемого", StateChosen, StateConfirmed, ActorGiver, ErrForbiddenTransition},
		{"владелец не акцептует за дарителя", StateConfirmed, StateAccepted, ActorOwner, ErrForbiddenTransition},
		{"владелец не резервирует", StateVisible, StateChosen, ActorOwner, ErrForbiddenTransition},
		{"система не подтверждает", StateChosen, StateConfirmed, ActorSystem, ErrForbiddenTransition},
		{"акцептованный не меняется", StateAccepted, StateVisible, ActorOwner, ErrInvalidTransition},
		{"отклонённый не возвращается", StateRejected, StateVisible, ActorOwner, ErrInvalidTransition},
		{"отклонённый не дарится", StateRejected, StateChosen, ActorGiver, ErrInvalidTransition},
		{"неизвестное состояние", State("WHATEVER"), StateVisible, ActorOwner, ErrInvalidTransition},
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

// TestTerminalStates фиксирует требование README: отклонённый подарок
// больше не доступен к дарению, акцептованный завершён.
func TestTerminalStates(t *testing.T) {
	for _, state := range []State{StateAccepted, StateRejected} {
		t.Run(string(state), func(t *testing.T) {
			if !state.Terminal() {
				t.Errorf("состояние %s должно быть терминальным", state)
			}
			for _, to := range []State{StateVisible, StateHidden, StateChosen, StateConfirmed} {
				for _, actor := range []Actor{ActorOwner, ActorGiver, ActorSystem} {
					if err := CanTransition(state, to, actor); err == nil {
						t.Errorf("%s разрешил переход в %s для %s", state, to, actor)
					}
				}
			}
		})
	}
}

func TestCreateItemValidate(t *testing.T) {
	tests := []struct {
		name    string
		item    CreateItem
		wantErr bool
	}{
		{"товар", CreateItem{Kind: KindProduct, Priority: 1,
			Provider: marketplace.ProviderStub, ProductId: "42"}, false},
		{"деньги", CreateItem{Kind: KindMoney, Priority: 3,
			Amount: 5_000_00, Title: "На велосипед"}, false},
		{"товар без идентификатора", CreateItem{Kind: KindProduct, Priority: 1,
			Provider: marketplace.ProviderStub}, true},
		{"товар без площадки", CreateItem{Kind: KindProduct, Priority: 1, ProductId: "42"}, true},
		{"товар с суммой", CreateItem{Kind: KindProduct, Priority: 1,
			Provider: marketplace.ProviderStub, ProductId: "42", Amount: 100}, true},
		{"деньги без суммы", CreateItem{Kind: KindMoney, Priority: 1, Title: "На велосипед"}, true},
		{"деньги без названия", CreateItem{Kind: KindMoney, Priority: 1, Amount: 100}, true},
		{"деньги с товаром", CreateItem{Kind: KindMoney, Priority: 1, Amount: 100,
			Title: "На велосипед", ProductId: "42"}, true},
		{"приоритет вне диапазона", CreateItem{Kind: KindMoney, Priority: 9,
			Amount: 100, Title: "На велосипед"}, true},
		{"неизвестный вид", CreateItem{Kind: "WHATEVER", Priority: 1}, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.item.Validate()
			if (err != nil) != test.wantErr {
				t.Errorf("Validate() = %v, ожидалась ошибка: %v", err, test.wantErr)
			}
		})
	}
}

// TestPublicHidesGiver фиксирует продуктовое требование: одаряемый узнаёт,
// что «кто-то» хочет вручить подарок, но не кто именно.
func TestPublicHidesGiver(t *testing.T) {
	giver := uuid.New()
	owner := uuid.New()
	item := Item{Id: uuid.New(), UserId: owner, GiverId: &giver, State: StateChosen}

	t.Run("владелец не видит дарителя", func(t *testing.T) {
		if public := item.Public(owner); public.GiverId != nil {
			t.Errorf("даритель виден владельцу: %s", public.GiverId)
		}
	})

	t.Run("даритель видит себя", func(t *testing.T) {
		if public := item.Public(giver); public.GiverId == nil || *public.GiverId != giver {
			t.Error("даритель не видит собственный резерв")
		}
	})
}
