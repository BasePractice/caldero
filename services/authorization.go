package services

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/grpc/metadata"
)

// Роли пользователя. Источник — claim токена, проброшенный шлюзом:
// хранить их в каждом сервисе значило бы дублировать одни и те же данные
// и расходиться при первом же изменении.
const (
	// RoleOperator позволяет действовать от имени другого пользователя —
	// например, оформлять ему кредит.
	RoleOperator = "operator"
	// RoleAdmin — административные операции.
	RoleAdmin = "admin"
	// RoleUser — роль обычного пользователя. Прав не добавляет и в базе
	// не хранится: она нужна, чтобы список ролей в токене никогда не был
	// пустым. Иначе шлюзу нечем перезаписать присланный клиентом заголовок
	// с ролями, и любой назначил бы себе роль сам.
	RoleUser = "user"
)

const (
	headerAuthorizedId = "X-Authorized-Id"
	headerRoles        = "X-Roles"
)

type AuthorizedUser struct {
	Id    uuid.UUID
	Roles []string
}

// HasRole сообщает, есть ли у пользователя роль. Администратор считается
// обладателем любой роли: иначе каждую проверку пришлось бы дублировать.
func (u AuthorizedUser) HasRole(role string) bool {
	return slices.Contains(u.Roles, role) || slices.Contains(u.Roles, RoleAdmin)
}

// CanActOnBehalfOf разрешает операцию над данными пользователя userId.
// Над своими данными пользователь работает всегда, над чужими — только
// с ролью оператора.
func (u AuthorizedUser) CanActOnBehalfOf(userId uuid.UUID) bool {
	return u.Id == userId || u.HasRole(RoleOperator)
}

func HttpAuthorized(request *http.Request) (*AuthorizedUser, error) {
	id, err := parseAuthorizedId(request.Header.Get(headerAuthorizedId))
	if err != nil {
		return nil, err
	}
	return &AuthorizedUser{Id: id, Roles: parseRoles(request.Header.Get(headerRoles))}, nil
}

func GrpcAuthorized(context context.Context) (*AuthorizedUser, error) {
	md, ok := metadata.FromIncomingContext(context)
	if !ok {
		return nil, fmt.Errorf("no metadata in request")
	}
	authorized := md.Get(strings.ToLower(headerAuthorizedId))
	if len(authorized) == 0 {
		return nil, fmt.Errorf("no authorized id")
	}
	id, err := parseAuthorizedId(authorized[0])
	if err != nil {
		return nil, err
	}
	return &AuthorizedUser{
		Id:    id,
		Roles: parseRoles(strings.Join(md.Get(strings.ToLower(headerRoles)), ",")),
	}, nil
}

func parseAuthorizedId(value string) (uuid.UUID, error) {
	id, err := uuid.Parse(value)
	if err != nil {
		return uuid.Nil, fmt.Errorf("invalid user id: %w", err)
	}
	// uuid.Parse принимает нулевой UUID, но пользователя с таким
	// идентификатором не существует.
	if id == uuid.Nil {
		return uuid.Nil, fmt.Errorf("invalid user id: was nil")
	}
	return id, nil
}

func parseRoles(value string) []string {
	if value == "" {
		return nil
	}
	roles := strings.Split(value, ",")
	result := make([]string, 0, len(roles))
	for _, role := range roles {
		if role = strings.TrimSpace(role); role != "" {
			result = append(result, role)
		}
	}
	return result
}
