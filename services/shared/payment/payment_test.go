package payment

import (
	"testing"

	"github.com/google/uuid"

	"wish/services/shared/credit"
)

func TestFeeFor(t *testing.T) {
	tests := []struct {
		name   string
		fee    Fee
		amount credit.Amount
		want   credit.Amount
	}{
		{"нулевое правило не берёт комиссии", Fee{}, 100_00, 0},
		{"доля от суммы", Fee{BasisPoints: 250}, 100_00, 2_50},
		{"доля округляется вверх", Fee{BasisPoints: 250}, 1_01, 3},
		{"фиксированная часть складывается с долей", Fee{BasisPoints: 100, Fixed: 10_00}, 100_00, 11_00},
		{"нижняя граница поднимает комиссию", Fee{BasisPoints: 10, Min: 5_00}, 100_00, 5_00},
		{"верхняя граница ограничивает комиссию", Fee{BasisPoints: 1000, Max: 50_00}, 10_000_00, 50_00},
		{"нулевая сумма не облагается", Fee{BasisPoints: 250, Fixed: 10_00}, 0, 0},
		{"предельная сумма не переполняет расчёт", Fee{BasisPoints: credit.BasisPointsInWhole}, MaxAmount, MaxAmount},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.fee.For(test.amount); got != test.want {
				t.Errorf("комиссия с %s = %s, ожидалось %s", test.amount, got, test.want)
			}
		})
	}
}

func TestFeeValidate(t *testing.T) {
	tests := []struct {
		name    string
		fee     Fee
		wantErr bool
	}{
		{"нулевое значение допустимо", Fee{}, false},
		{"обычный тариф", Fee{BasisPoints: 250, Fixed: 10_00, Min: 5_00, Max: 500_00}, false},
		{"доля больше 100 %", Fee{BasisPoints: credit.BasisPointsInWhole + 1}, true},
		{"отрицательная фиксированная часть", Fee{Fixed: -1}, true},
		{"верхняя граница ниже нижней", Fee{Min: 10_00, Max: 5_00}, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.fee.Validate()
			if (err != nil) != test.wantErr {
				t.Errorf("Validate() = %v, ожидалась ошибка: %v", err, test.wantErr)
			}
		})
	}
}

func TestStatusTransitions(t *testing.T) {
	tests := []struct {
		name string
		from Status
		to   Status
		want bool
	}{
		{"ожидание остаётся ожиданием", StatusPending, StatusPending, true},
		{"ожидание завершается успехом", StatusPending, StatusSucceeded, true},
		{"ожидание завершается отказом", StatusPending, StatusFailed, true},
		{"успех не пересматривается", StatusSucceeded, StatusFailed, false},
		{"отказ не пересматривается", StatusFailed, StatusSucceeded, false},
		{"успех не возвращается в ожидание", StatusSucceeded, StatusPending, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.from.CanFollow(test.to); got != test.want {
				t.Errorf("%s → %s = %v, ожидалось %v", test.from, test.to, got, test.want)
			}
		})
	}
}

func TestRequestValidation(t *testing.T) {
	user := uuid.New()

	deposits := []struct {
		name    string
		request DepositRequest
		wantErr bool
	}{
		{"корректное пополнение", DepositRequest{
			IdempotencyKey: "key", UserId: user, Amount: 100_00, Method: MethodSBP}, false},
		{"без ключа идемпотентности", DepositRequest{
			UserId: user, Amount: 100_00, Method: MethodSBP}, true},
		{"нулевая сумма", DepositRequest{
			IdempotencyKey: "key", UserId: user, Method: MethodSBP}, true},
		{"сумма сверх предела", DepositRequest{
			IdempotencyKey: "key", UserId: user, Amount: MaxAmount + 1, Method: MethodSBP}, true},
		{"неизвестный способ", DepositRequest{
			IdempotencyKey: "key", UserId: user, Amount: 100_00, Method: "CASH"}, true},
	}
	for _, test := range deposits {
		t.Run("пополнение: "+test.name, func(t *testing.T) {
			err := test.request.Validate()
			if (err != nil) != test.wantErr {
				t.Errorf("Validate() = %v, ожидалась ошибка: %v", err, test.wantErr)
			}
		})
	}

	payouts := []struct {
		name    string
		request PayoutRequest
		wantErr bool
	}{
		{"выплата через СБП", PayoutRequest{
			IdempotencyKey: "key", UserId: user, Amount: 100_00, Method: MethodSBP, Phone: "+79000000000"}, false},
		{"СБП без телефона", PayoutRequest{
			IdempotencyKey: "key", UserId: user, Amount: 100_00, Method: MethodSBP}, true},
		{"выплата на карту без токена", PayoutRequest{
			IdempotencyKey: "key", UserId: user, Amount: 100_00, Method: MethodCard}, true},
		{"выплата на карту по токену", PayoutRequest{
			IdempotencyKey: "key", UserId: user, Amount: 100_00, Method: MethodCard, CardToken: "tok"}, false},
	}
	for _, test := range payouts {
		t.Run("выплата: "+test.name, func(t *testing.T) {
			err := test.request.Validate()
			if (err != nil) != test.wantErr {
				t.Errorf("Validate() = %v, ожидалась ошибка: %v", err, test.wantErr)
			}
		})
	}
}
