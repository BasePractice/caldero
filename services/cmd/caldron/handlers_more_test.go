package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wish/services/shared/caldron"

	"github.com/google/uuid"
)

func call(handler http.Handler, method, path, body string, user uuid.UUID) *httptest.ResponseRecorder {
	var request *http.Request
	if body == "" {
		request = httptest.NewRequest(method, path, nil)
	} else {
		request = httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
	}
	if user != uuid.Nil {
		request = authorized(request, user)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestListCaldronsHandler(t *testing.T) {
	env := newEnvironment(t)
	handler := registerHttpHandlers(env.caldrons)
	creator := uuid.New()
	member := uuid.New()
	pot := env.fixedCaldron(t, creator, 2_500_00, member)

	for _, user := range []uuid.UUID{creator, member} {
		recorder := call(handler, http.MethodGet, "/caldrons", "", user)
		if recorder.Code != http.StatusOK {
			t.Fatalf("код ответа %d (%s)", recorder.Code, recorder.Body)
		}
		var caldrons []caldron.Caldron
		if err := json.Unmarshal(recorder.Body.Bytes(), &caldrons); err != nil {
			t.Fatalf("разбор ответа: %v", err)
		}
		if len(caldrons) != 1 || caldrons[0].Id != pot.Id {
			t.Errorf("список котлов пользователя %s: %+v", user, caldrons)
		}
	}

	t.Run("посторонний котла не видит", func(t *testing.T) {
		recorder := call(handler, http.MethodGet, "/caldrons", "", uuid.New())
		if recorder.Code != http.StatusOK {
			t.Fatalf("код ответа %d (%s)", recorder.Code, recorder.Body)
		}
		var caldrons []caldron.Caldron
		if err := json.Unmarshal(recorder.Body.Bytes(), &caldrons); err != nil {
			t.Fatalf("разбор ответа: %v", err)
		}
		if len(caldrons) != 0 {
			t.Errorf("постороннему видно %d котлов", len(caldrons))
		}
	})
}

// TestRemoveParticipantHandler: пока сбор не начался, создатель вправе
// убрать участника — состав котла на этом этапе ещё меняется.
func TestRemoveParticipantHandler(t *testing.T) {
	env := newEnvironment(t)
	handler := registerHttpHandlers(env.caldrons)
	creator := uuid.New()
	member := uuid.New()
	pot := env.fixedCaldron(t, creator, 2_500_00, member)
	path := "/caldrons/" + pot.Id.String() + "/participants/"

	recorder := call(handler, http.MethodDelete, path+member.String(), "", creator)
	if recorder.Code != http.StatusOK {
		t.Fatalf("код ответа %d (%s)", recorder.Code, recorder.Body)
	}
	var updated caldron.Caldron
	if err := json.Unmarshal(recorder.Body.Bytes(), &updated); err != nil {
		t.Fatalf("разбор ответа: %v", err)
	}
	if updated.IsParticipant(member) {
		t.Error("участник остался в котле")
	}

	tests := []struct {
		name string
		path string
		user uuid.UUID
		want int
	}{
		{"без токена", path + member.String(), uuid.Nil, http.StatusUnauthorized},
		{"неразбираемый котёл", "/caldrons/не-uuid/participants/" + member.String(), creator, http.StatusBadRequest},
		{"неразбираемый участник", path + "не-uuid", creator, http.StatusBadRequest},
		// Чужой котёл отдаётся как несуществующий: иначе перебором можно
		// узнать, какие котлы есть.
		{"посторонний убирает участника", path + member.String(), uuid.New(), http.StatusNotFound},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if code := call(handler, http.MethodDelete, test.path, "", test.user).Code; code != test.want {
				t.Errorf("код ответа %d, ожидался %d", code, test.want)
			}
		})
	}
}

func TestGiftsHandler(t *testing.T) {
	env := newEnvironment(t)
	handler := registerHttpHandlers(env.caldrons)
	creator := uuid.New()
	member := uuid.New()
	env.wallet.fund(creator, 100_000_00)
	env.wallet.fund(member, 100_000_00)
	// Взнос подобран под цену товара заглушки: список подарков не может
	// стоить дороже, чем котёл соберёт.
	pot := env.fixedCaldron(t, creator, 20_000_00, member)
	path := "/caldrons/" + pot.Id.String() + "/gifts"

	body := `[{"provider":"STUB","product_id":"coffee-machine"}]`
	if r := call(handler, http.MethodPut, path, body, creator); r.Code != http.StatusOK {
		t.Fatalf("сохранение списка подарков: код ответа %d (%s)", r.Code, r.Body)
	}

	recorder := call(handler, http.MethodGet, path, "", creator)
	if recorder.Code != http.StatusOK {
		t.Fatalf("код ответа %d (%s)", recorder.Code, recorder.Body)
	}
	var gifts []caldron.Gift
	if err := json.Unmarshal(recorder.Body.Bytes(), &gifts); err != nil {
		t.Fatalf("разбор ответа: %v", err)
	}
	if len(gifts) != 1 || gifts[0].ProductId != "coffee-machine" {
		t.Errorf("список подарков: %+v", gifts)
	}

	// Чужие списки не показываются: розыгрыш иначе перестаёт быть сюрпризом.
	t.Run("участник видит только свой список", func(t *testing.T) {
		recorder := call(handler, http.MethodGet, path, "", member)
		if recorder.Code != http.StatusOK {
			t.Fatalf("код ответа %d (%s)", recorder.Code, recorder.Body)
		}
		var others []caldron.Gift
		if err := json.Unmarshal(recorder.Body.Bytes(), &others); err != nil {
			t.Fatalf("разбор ответа: %v", err)
		}
		if len(others) != 0 {
			t.Errorf("участнику виден чужой список: %+v", others)
		}
	})

	tests := []struct {
		name string
		user uuid.UUID
		want int
	}{
		{"без токена", uuid.Nil, http.StatusUnauthorized},
		{"посторонний", uuid.New(), http.StatusNotFound},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if code := call(handler, http.MethodGet, path, "", test.user).Code; code != test.want {
				t.Errorf("код ответа %d, ожидался %d", code, test.want)
			}
		})
	}
}

func TestSetGiftsErrors(t *testing.T) {
	env := newEnvironment(t)
	handler := registerHttpHandlers(env.caldrons)
	creator := uuid.New()
	pot := env.fixedCaldron(t, creator, 2_500_00, uuid.New())
	path := "/caldrons/" + pot.Id.String() + "/gifts"

	tests := []struct {
		name string
		body string
		user uuid.UUID
		want int
	}{
		{"без токена", `[]`, uuid.Nil, http.StatusUnauthorized},
		{"нечитаемое тело", `[{"provider":`, creator, http.StatusBadRequest},
		{"посторонний", `[{"provider":"STUB","product_id":"x"}]`, uuid.New(), http.StatusNotFound},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if code := call(handler, http.MethodPut, path, test.body, test.user).Code; code != test.want {
				t.Errorf("код ответа %d, ожидался %d", code, test.want)
			}
		})
	}
}

func TestSetArbiterErrors(t *testing.T) {
	env := newEnvironment(t)
	handler := registerHttpHandlers(env.caldrons)
	creator := uuid.New()
	member := uuid.New()
	pot := env.fixedCaldron(t, creator, 2_500_00, member)
	path := "/caldrons/" + pot.Id.String() + "/arbiter"

	if code := call(handler, http.MethodPut, path,
		`{"user_id":"`+member.String()+`"}`, creator).Code; code != http.StatusOK {
		t.Fatalf("назначение арбитра: код ответа %d", code)
	}

	tests := []struct {
		name string
		body string
		user uuid.UUID
		want int
	}{
		{"без токена", `{"user_id":"` + member.String() + `"}`, uuid.Nil, http.StatusUnauthorized},
		{"нечитаемое тело", `{"user_id":`, creator, http.StatusBadRequest},
		// Арбитр выбирается из участников: посторонний не может судить
		// чужой розыгрыш.
		{"арбитр не участвует в котле", `{"user_id":"` + uuid.NewString() + `"}`, creator, http.StatusForbidden},
		{"назначает не создатель", `{"user_id":"` + member.String() + `"}`, member, http.StatusForbidden},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if code := call(handler, http.MethodPut, path, test.body, test.user).Code; code != test.want {
				t.Errorf("код ответа %d, ожидался %d", code, test.want)
			}
		})
	}
}

func TestRequestValidation(t *testing.T) {
	env := newEnvironment(t)
	handler := registerHttpHandlers(env.caldrons)
	user := uuid.New()

	tests := []struct {
		name   string
		method string
		path   string
		user   uuid.UUID
		want   int
	}{
		{"котёл без токена", http.MethodGet, "/caldrons/" + uuid.NewString(), uuid.Nil, http.StatusUnauthorized},
		{"неразбираемый идентификатор", http.MethodGet, "/caldrons/не-uuid", user, http.StatusBadRequest},
		{"неизвестный котёл", http.MethodGet, "/caldrons/" + uuid.NewString(), user, http.StatusNotFound},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if code := call(handler, test.method, test.path, "", test.user).Code; code != test.want {
				t.Errorf("код ответа %d, ожидался %d", code, test.want)
			}
		})
	}
}

func TestAddParticipantErrors(t *testing.T) {
	env := newEnvironment(t)
	handler := registerHttpHandlers(env.caldrons)
	creator := uuid.New()
	pot := env.fixedCaldron(t, creator, 2_500_00)
	path := "/caldrons/" + pot.Id.String() + "/participants"

	tests := []struct {
		name string
		body string
		user uuid.UUID
		want int
	}{
		{"без токена", `{"user_id":"` + uuid.NewString() + `"}`, uuid.Nil, http.StatusUnauthorized},
		{"нечитаемое тело", `{"user_id":`, creator, http.StatusBadRequest},
		{"добавляет не создатель", `{"user_id":"` + uuid.NewString() + `"}`, uuid.New(), http.StatusNotFound},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := call(handler, http.MethodPost, path, test.body, test.user)
			if recorder.Code != test.want {
				t.Errorf("код ответа %d, ожидался %d (%s)", recorder.Code, test.want, recorder.Body)
			}
		})
	}

	// Заявка без участника отвергается — но кодом 500 вместо 400: проверка
	// требует режима котла и потому живёт в сервисе, а её ошибка не разобрана
	// в writeError. Исправление — T-084.
	t.Run("без участника", func(t *testing.T) {
		if code := call(handler, http.MethodPost, path, `{}`, creator).Code; code < 400 {
			t.Errorf("код ответа %d: заявка без участника принята", code)
		}
	})
}
