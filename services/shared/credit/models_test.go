package credit

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestAmountString(t *testing.T) {
	tests := []struct {
		name   string
		amount Amount
		want   string
	}{
		{"ноль", 0, "0.00"},
		{"копейки без рублей", 7, "0.07"},
		{"десятки копеек", 70, "0.70"},
		{"рубли и копейки", 123456, "1234.56"},
		{"целые рубли", 100, "1.00"},
		{"отрицательная сумма", -123456, "-1234.56"},
		{"отрицательные копейки", -7, "-0.07"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.amount.String(); got != test.want {
				t.Errorf("получено %q, ожидалось %q", got, test.want)
			}
		})
	}
}

func TestRateString(t *testing.T) {
	tests := []struct {
		name string
		rate Rate
		want string
	}{
		{"целые проценты", 1200, "12.00%"},
		{"с половиной процента", 1250, "12.50%"},
		{"минимальная ставка", MinRate, "1.00%"},
		{"максимальная ставка", MaxRate, "300.00%"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.rate.String(); got != test.want {
				t.Errorf("получено %q, ожидалось %q", got, test.want)
			}
		})
	}
}

// valid — заявка, проходящая проверку. Тесты меняют в ней ровно одно поле,
// чтобы отказ нельзя было объяснить чем-то ещё.
func valid() CreateCredit {
	return CreateCredit{
		UserId:      uuid.New(),
		Type:        TypeSimple,
		Kind:        KindAnnuity,
		Month:       12,
		Rate:        1250,
		Balance:     100_000,
		AlreadyPaid: 0,
	}
}

func TestCreateCreditValidate(t *testing.T) {
	tests := []struct {
		name    string
		change  func(*CreateCredit)
		wantErr bool
	}{
		{"корректная заявка", func(*CreateCredit) {}, false},
		{"максимальный срок", func(c *CreateCredit) { c.Month = MaxMonth }, false},
		{"минимальный срок", func(c *CreateCredit) { c.Month = 2 }, false},
		{"дифференцированные платежи", func(c *CreateCredit) { c.Kind = KindDifferentiated }, false},
		{"ипотека", func(c *CreateCredit) { c.Type = TypeMortgage }, false},
		{"микрозаём", func(c *CreateCredit) { c.Type = TypeMicro }, false},
		{"часть уже выплачена", func(c *CreateCredit) { c.AlreadyPaid = 99_999 }, false},

		{"без пользователя", func(c *CreateCredit) { c.UserId = uuid.Nil }, true},
		{"неизвестный тип", func(c *CreateCredit) { c.Type = "WHATEVER" }, true},
		{"неизвестный вид платежей", func(c *CreateCredit) { c.Kind = "WHATEVER" }, true},
		{"ставка ниже минимума", func(c *CreateCredit) { c.Rate = MinRate - 1 }, true},
		{"ставка выше максимума", func(c *CreateCredit) { c.Rate = MaxRate + 1 }, true},
		{"нулевая сумма", func(c *CreateCredit) { c.Balance = 0 }, true},
		{"отрицательная сумма", func(c *CreateCredit) { c.Balance = -1 }, true},
		{"отрицательная выплата", func(c *CreateCredit) { c.AlreadyPaid = -1 }, true},
		{"выплачено больше суммы", func(c *CreateCredit) { c.AlreadyPaid = c.Balance }, true},
		{"срок в один месяц", func(c *CreateCredit) { c.Month = 1 }, true},
		{"срок больше предельного", func(c *CreateCredit) { c.Month = MaxMonth + 1 }, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := valid()
			test.change(&c)

			err := c.Validate()
			if test.wantErr && err == nil {
				t.Error("заявка принята, ожидался отказ")
			}
			if !test.wantErr && err != nil {
				t.Errorf("заявка отклонена: %v", err)
			}
		})
	}
}

func TestCreateCreditString(t *testing.T) {
	c := valid()
	c.Balance = 100_000
	c.Rate = 1250

	got := c.String()
	for _, want := range []string{c.UserId.String(), TypeSimple, "1000.00", "12.50%"} {
		if !strings.Contains(got, want) {
			t.Errorf("в %q нет %q", got, want)
		}
	}
}
