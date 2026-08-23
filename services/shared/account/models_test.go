package account

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestCreateAccountValidate(t *testing.T) {
	creditId := uuid.New()

	tests := []struct {
		name    string
		account CreateAccount
		wantErr bool
	}{
		{
			name:    "дебетовый счёт без кредита",
			account: CreateAccount{UserId: uuid.New(), Type: TypeDebit},
		},
		{
			name:    "кредитный счёт со ссылкой на кредит",
			account: CreateAccount{UserId: uuid.New(), Type: TypeCredit, CreditId: &creditId},
		},
		{
			name:    "без пользователя",
			account: CreateAccount{Type: TypeDebit},
			wantErr: true,
		},
		{
			name:    "дебетовый счёт со ссылкой на кредит",
			account: CreateAccount{UserId: uuid.New(), Type: TypeDebit, CreditId: &creditId},
			wantErr: true,
		},
		{
			name:    "кредитный счёт без ссылки на кредит",
			account: CreateAccount{UserId: uuid.New(), Type: TypeCredit},
			wantErr: true,
		},
		{
			name:    "неизвестный тип счёта",
			account: CreateAccount{UserId: uuid.New(), Type: "WHATEVER"},
			wantErr: true,
		},
		{
			name:    "пустой тип счёта",
			account: CreateAccount{UserId: uuid.New()},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.account.Validate()
			if test.wantErr && err == nil {
				t.Error("заявка принята, ожидался отказ")
			}
			if !test.wantErr && err != nil {
				t.Errorf("заявка отклонена: %v", err)
			}
		})
	}
}

func TestCreateAccountString(t *testing.T) {
	a := CreateAccount{UserId: uuid.New(), Type: TypeCredit}

	got := a.String()
	for _, want := range []string{a.UserId.String(), TypeCredit} {
		if !strings.Contains(got, want) {
			t.Errorf("в %q нет %q", got, want)
		}
	}
}
