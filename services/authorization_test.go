package services

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/metadata"
)

const validId = "0f95e97c-0ea4-476f-9146-d015ec22e240"

func TestHttpAuthorized(t *testing.T) {
	tests := []struct {
		name      string
		headers   map[string]string
		wantId    string
		wantRoles []string
		wantErr   bool
	}{
		{
			name:    "валидный идентификатор",
			headers: map[string]string{"X-Authorized-Id": validId},
			wantId:  validId,
		},
		{
			// Заголовки в net/http нечувствительны к регистру, и полагаться
			// на конкретное написание со стороны шлюза нельзя.
			name:    "другой регистр заголовка",
			headers: map[string]string{"x-authorized-id": validId},
			wantId:  validId,
		},
		{
			name:      "роли перечислены через запятую",
			headers:   map[string]string{"X-Authorized-Id": validId, "X-Roles": "operator, admin"},
			wantId:    validId,
			wantRoles: []string{"operator", "admin"},
		},
		{
			name:      "пустые элементы в списке ролей отбрасываются",
			headers:   map[string]string{"X-Authorized-Id": validId, "X-Roles": "operator,,"},
			wantId:    validId,
			wantRoles: []string{"operator"},
		},
		{
			name:    "заголовка нет",
			headers: map[string]string{},
			wantErr: true,
		},
		{
			name:    "мусор вместо идентификатора",
			headers: map[string]string{"X-Authorized-Id": "not-a-uuid"},
			wantErr: true,
		},
		{
			// uuid.Parse принимает нулевой UUID, но такого пользователя нет.
			name:    "нулевой идентификатор",
			headers: map[string]string{"X-Authorized-Id": uuid.Nil.String()},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			for name, value := range tt.headers {
				request.Header.Set(name, value)
			}

			user, err := HttpAuthorized(request)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ожидалась ошибка, получен пользователь %v", user)
				}
				return
			}
			if err != nil {
				t.Fatalf("неожиданная ошибка: %v", err)
			}
			if user.Id.String() != tt.wantId {
				t.Errorf("id = %s, ожидался %s", user.Id, tt.wantId)
			}
			if !slices.Equal(user.Roles, tt.wantRoles) {
				t.Errorf("роли = %v, ожидались %v", user.Roles, tt.wantRoles)
			}
		})
	}
}

func TestGrpcAuthorized(t *testing.T) {
	tests := []struct {
		name      string
		md        metadata.MD
		wantRoles []string
		wantErr   bool
	}{
		{
			name: "валидный идентификатор",
			md:   metadata.Pairs("x-authorized-id", validId),
		},
		{
			name:      "роли из нескольких значений метаданных",
			md:        metadata.Pairs("x-authorized-id", validId, "x-roles", "operator", "x-roles", "admin"),
			wantRoles: []string{"operator", "admin"},
		},
		{
			name:    "метаданных нет",
			md:      nil,
			wantErr: true,
		},
		{
			name:    "идентификатора нет",
			md:      metadata.Pairs("x-roles", "operator"),
			wantErr: true,
		},
		{
			name:    "мусор вместо идентификатора",
			md:      metadata.Pairs("x-authorized-id", "not-a-uuid"),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.md != nil {
				ctx = metadata.NewIncomingContext(ctx, tt.md)
			}

			user, err := GrpcAuthorized(ctx)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ожидалась ошибка, получен пользователь %v", user)
				}
				return
			}
			if err != nil {
				t.Fatalf("неожиданная ошибка: %v", err)
			}
			if user.Id.String() != validId {
				t.Errorf("id = %s, ожидался %s", user.Id, validId)
			}
			if !slices.Equal(user.Roles, tt.wantRoles) {
				t.Errorf("роли = %v, ожидались %v", user.Roles, tt.wantRoles)
			}
		})
	}
}

func TestAuthorizedUserPermissions(t *testing.T) {
	self := uuid.MustParse(validId)
	other := uuid.MustParse("7ef4bae6-e4fa-4956-8f61-956907b2404f")

	tests := []struct {
		name         string
		roles        []string
		target       uuid.UUID
		wantCanAct   bool
		wantOperator bool
	}{
		{name: "над собой без ролей", target: self, wantCanAct: true},
		{name: "над чужим без ролей", target: other, wantCanAct: false},
		{name: "над чужим с ролью оператора", roles: []string{"operator"}, target: other, wantCanAct: true, wantOperator: true},
		{
			// Администратор считается обладателем любой роли, иначе каждую
			// проверку пришлось бы дублировать.
			name: "над чужим с ролью администратора", roles: []string{"admin"}, target: other,
			wantCanAct: true, wantOperator: true,
		},
		{name: "посторонняя роль не даёт прав", roles: []string{"guest"}, target: other},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := AuthorizedUser{Id: self, Roles: tt.roles}
			if got := user.CanActOnBehalfOf(tt.target); got != tt.wantCanAct {
				t.Errorf("CanActOnBehalfOf = %v, ожидалось %v", got, tt.wantCanAct)
			}
			if got := user.HasRole(RoleOperator); got != tt.wantOperator {
				t.Errorf("HasRole(operator) = %v, ожидалось %v", got, tt.wantOperator)
			}
		})
	}
}
