//go:build integration

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wish/services"

	"github.com/google/uuid"
)

func patch(handler http.Handler, body, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPatch, "/profile", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// TestUpdateProfileValidation проверяет разбор полей профиля: телефон
// нормализуется, а почта и пол проверяются до записи в базу.
func TestUpdateProfileValidation(t *testing.T) {
	_, handler := newOAuth2Service(t)
	clientId := createClient(t, handler)
	username := "user-" + uuid.NewString()[:8]
	registerViaAPI(t, handler, username, "+79004440011")
	token := tokenFor(t, handler, clientId, username)

	t.Run("телефон приводится к E.164", func(t *testing.T) {
		recorder := patch(handler, `{"phone":"8 (900) 444-00-22"}`, token)
		if recorder.Code != http.StatusOK {
			t.Fatalf("код ответа %d (%s)", recorder.Code, recorder.Body)
		}
		var profile map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &profile); err != nil {
			t.Fatalf("разбор ответа: %v", err)
		}
		if profile["phone"] != "+79004440022" {
			t.Errorf("телефон %v, ожидался +79004440022", profile["phone"])
		}
	})

	tests := []struct {
		name string
		body string
		want int
	}{
		{"неразбираемый телефон", `{"phone":"тел"}`, http.StatusBadRequest},
		// Телефон обязателен, поэтому очистить его нельзя.
		{"очистка телефона", `{"phone":""}`, http.StatusBadRequest},
		{"некорректная почта", `{"email":"не-почта"}`, http.StatusBadRequest},
		{"неизвестный пол", `{"gender":"WHATEVER"}`, http.StatusBadRequest},
		{"нечитаемое тело", `{"phone":`, http.StatusBadRequest},
		{"корректная почта", `{"email":"new@example.com"}`, http.StatusOK},
		{"корректный пол", `{"gender":"FEMALE"}`, http.StatusOK},
		{"пустое обновление", `{}`, http.StatusOK},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := patch(handler, test.body, token)
			if recorder.Code != test.want {
				t.Errorf("код ответа %d, ожидался %d (%s)", recorder.Code, test.want, recorder.Body)
			}
		})
	}
}

// TestUpdateProfileConflict: занятые телефон и почта — это конфликт,
// а не внутренняя ошибка.
func TestUpdateProfileConflict(t *testing.T) {
	_, handler := newOAuth2Service(t)
	clientId := createClient(t, handler)

	first := "user-" + uuid.NewString()[:8]
	registerViaAPI(t, handler, first, "+79004440033")

	second := "user-" + uuid.NewString()[:8]
	registerViaAPI(t, handler, second, "+79004440044")
	token := tokenFor(t, handler, clientId, second)

	recorder := patch(handler, `{"phone":"+79004440033"}`, token)
	if recorder.Code != http.StatusConflict {
		t.Errorf("код ответа %d, ожидался %d (%s)", recorder.Code, http.StatusConflict, recorder.Body)
	}
}

func TestUpdateProfileUnauthorized(t *testing.T) {
	_, handler := newOAuth2Service(t)

	if code := patch(handler, `{"gender":"MALE"}`, "").Code; code != http.StatusUnauthorized {
		t.Errorf("код ответа %d, ожидался %d", code, http.StatusUnauthorized)
	}
}

// TestRolesInToken фиксирует передачу ролей: шлюз пробрасывает их
// в заголовок, и сервисы за ним не ходят за ролями в базу — иначе одни
// и те же данные пришлось бы держать в каждом сервисе.
//
// Роль есть в токене всегда, даже когда особых ролей нет: пустым claim
// шлюзу нечем перезаписать присланный клиентом заголовок, и пользователь
// назначил бы себе роль сам.
func TestRolesInToken(t *testing.T) {
	service, handler := newOAuth2Service(t)
	clientId := createClient(t, handler)

	plain := "user-" + uuid.NewString()[:8]
	registerViaAPI(t, handler, plain, "+79004440055")
	roles := claimRoles(t, tokenFor(t, handler, clientId, plain))
	if len(roles) != 1 || roles[0] != services.RoleUser {
		t.Errorf("у обычного пользователя роли %v, ожидалась только %s", roles, services.RoleUser)
	}

	operator := "user-" + uuid.NewString()[:8]
	operatorId := registerViaAPI(t, handler, operator, "+79004440066")
	store, ok := service.db.(*ds)
	if !ok {
		t.Fatalf("репозиторий имеет тип %T", service.db)
	}
	if _, err := store.db.ExecContext(context.Background(),
		"INSERT INTO user_roles (user_id, role) VALUES ($1, $2)",
		operatorId, "operator"); err != nil {
		t.Fatalf("назначение роли: %v", err)
	}

	roles = claimRoles(t, tokenFor(t, handler, clientId, operator))
	if len(roles) != 2 || roles[0] != services.RoleUser || roles[1] != services.RoleOperator {
		t.Errorf("роли в токене %v, ожидались %s и %s", roles, services.RoleUser, services.RoleOperator)
	}
}

// claimRoles достаёт роли из полезной нагрузки access-токена: он подписан
// RS256, и его содержимое читается без ключа.
func claimRoles(t *testing.T, token string) []string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("токен не похож на JWT: %d частей", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("разбор полезной нагрузки: %v", err)
	}
	var claims struct {
		Roles []string `json:"roles"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("разбор claims: %v", err)
	}
	return claims.Roles
}

// TestProfileAfterUserRemoved: токен переживает своего владельца — он
// подписан и действует до истечения. Обработчик обязан ответить «нет
// такого», а не пятисоткой.
func TestProfileAfterUserRemoved(t *testing.T) {
	service, handler := newOAuth2Service(t)
	clientId := createClient(t, handler)
	username := "user-" + uuid.NewString()[:8]
	userId := registerViaAPI(t, handler, username, "+79004440077")
	token := tokenFor(t, handler, clientId, username)

	store, ok := service.db.(*ds)
	if !ok {
		t.Fatalf("репозиторий имеет тип %T", service.db)
	}
	if _, err := store.db.ExecContext(context.Background(),
		"DELETE FROM users WHERE user_id = $1", userId); err != nil {
		t.Fatalf("удаление пользователя: %v", err)
	}

	for _, path := range []string{"/profile", "/profile/identities"} {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.Header.Set("Authorization", "Bearer "+token)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusNotFound {
				t.Errorf("код ответа %d, ожидался %d (%s)",
					recorder.Code, http.StatusNotFound, recorder.Body)
			}
		})
	}
}
