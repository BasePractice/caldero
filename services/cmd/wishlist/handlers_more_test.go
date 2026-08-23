package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wish/services/shared/payment"
	"wish/services/shared/wishlist"

	"github.com/google/uuid"
)

func do(handler http.Handler, method, path, body string, user uuid.UUID) *httptest.ResponseRecorder {
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

// TestChosenHandler: даритель должен видеть, что он уже выбрал, — иначе
// он не знает, какие подарки за ним числятся.
func TestChosenHandler(t *testing.T) {
	env := newTestEnvironment(t, payment.Fee{}, nil)
	handler := registerHttpHandlers(env.gifts, env.shopaholic)
	owner := uuid.New()
	giver := uuid.New()
	item := env.addProduct(t, owner)

	if code := do(handler, http.MethodPost,
		"/wishlist/items/"+item.Id.String()+"/reserve", "", giver).Code; code != http.StatusOK {
		t.Fatalf("резерв: код ответа %d", code)
	}

	recorder := do(handler, http.MethodGet, "/wishlist/chosen", "", giver)
	if recorder.Code != http.StatusOK {
		t.Fatalf("код ответа %d (%s)", recorder.Code, recorder.Body)
	}
	var items []wishlist.Item
	if err := json.Unmarshal(recorder.Body.Bytes(), &items); err != nil {
		t.Fatalf("разбор ответа: %v", err)
	}
	if len(items) != 1 || items[0].Id != item.Id {
		t.Errorf("выбранные подарки: %+v", items)
	}

	t.Run("без токена", func(t *testing.T) {
		if code := do(handler, http.MethodGet, "/wishlist/chosen", "", uuid.Nil).Code; code != http.StatusUnauthorized {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusUnauthorized)
		}
	})
}

func TestRemoveHandler(t *testing.T) {
	env := newTestEnvironment(t, payment.Fee{}, nil)
	handler := registerHttpHandlers(env.gifts, env.shopaholic)
	owner := uuid.New()
	item := env.addProduct(t, owner)

	if code := do(handler, http.MethodDelete,
		"/wishlist/items/"+item.Id.String(), "", owner).Code; code != http.StatusNoContent {
		t.Fatalf("удаление: код ответа %d", code)
	}

	tests := []struct {
		name string
		path string
		user uuid.UUID
		want int
	}{
		{"без токена", "/wishlist/items/" + item.Id.String(), uuid.Nil, http.StatusUnauthorized},
		{"неразбираемый идентификатор", "/wishlist/items/не-uuid", owner, http.StatusBadRequest},
		// Удалённый элемент неотличим от чужого: иначе перебором можно
		// узнать, какие элементы вообще существуют.
		{"уже удалён", "/wishlist/items/" + item.Id.String(), owner, http.StatusNotFound},
		{"чужой элемент", "/wishlist/items/" + uuid.NewString(), uuid.New(), http.StatusNotFound},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if code := do(handler, http.MethodDelete, test.path, "", test.user).Code; code != test.want {
				t.Errorf("код ответа %d, ожидался %d", code, test.want)
			}
		})
	}
}

// TestShopHandler проходит шопоголика по HTTP: случайный набор покупок
// в пределах бюджета, история прогонов и один прогон по идентификатору.
func TestShopHandler(t *testing.T) {
	env := newTestEnvironment(t, payment.Fee{}, nil)
	handler := registerHttpHandlers(env.gifts, env.shopaholic)
	buyer := uuid.New()
	env.wallet.fund(buyer, 1_000_000)

	body := `{"budget":500000,"items":[{"provider":"STUB","product_id":"coffee-machine"},` +
		`{"provider":"STUB","product_id":"headphones"}]}`
	recorder := do(handler, http.MethodPost, "/shopping", body, buyer)
	if recorder.Code != http.StatusOK {
		t.Fatalf("код ответа %d (%s)", recorder.Code, recorder.Body)
	}
	if recorder.Header().Get("X-Run-Id") == "" {
		t.Error("идентификатор прогона не возвращён")
	}

	var run wishlist.Run
	if err := json.Unmarshal(recorder.Body.Bytes(), &run); err != nil {
		t.Fatalf("разбор ответа: %v", err)
	}
	if run.Spent > 500_000 {
		t.Errorf("потрачено %s при бюджете 5000.00", run.Spent)
	}

	t.Run("история прогонов", func(t *testing.T) {
		recorder := do(handler, http.MethodGet, "/shopping", "", buyer)
		if recorder.Code != http.StatusOK {
			t.Fatalf("код ответа %d (%s)", recorder.Code, recorder.Body)
		}
		var runs []wishlist.Run
		if err := json.Unmarshal(recorder.Body.Bytes(), &runs); err != nil {
			t.Fatalf("разбор ответа: %v", err)
		}
		if len(runs) != 1 || runs[0].Id != run.Id {
			t.Errorf("история прогонов: %+v", runs)
		}
	})

	t.Run("один прогон", func(t *testing.T) {
		recorder := do(handler, http.MethodGet, "/shopping/"+run.Id.String(), "", buyer)
		if recorder.Code != http.StatusOK {
			t.Fatalf("код ответа %d (%s)", recorder.Code, recorder.Body)
		}
		var loaded wishlist.Run
		if err := json.Unmarshal(recorder.Body.Bytes(), &loaded); err != nil {
			t.Fatalf("разбор ответа: %v", err)
		}
		if loaded.Id != run.Id {
			t.Errorf("прогон %s, ожидался %s", loaded.Id, run.Id)
		}
	})

	t.Run("чужой прогон не виден", func(t *testing.T) {
		code := do(handler, http.MethodGet, "/shopping/"+run.Id.String(), "", uuid.New()).Code
		if code != http.StatusNotFound {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusNotFound)
		}
	})
}

func TestShopHandlerErrors(t *testing.T) {
	env := newTestEnvironment(t, payment.Fee{}, nil)
	handler := registerHttpHandlers(env.gifts, env.shopaholic)
	buyer := uuid.New()

	tests := []struct {
		name   string
		method string
		path   string
		body   string
		user   uuid.UUID
		want   int
	}{
		{
			name: "запуск без токена", method: http.MethodPost, path: "/shopping",
			body: `{"budget":1000,"items":[{"provider":"STUB","product_id":"x"}]}`,
			user: uuid.Nil, want: http.StatusUnauthorized,
		},
		{
			name: "нечитаемое тело", method: http.MethodPost, path: "/shopping",
			body: `{"budget":`, user: buyer, want: http.StatusBadRequest,
		},
		{
			name: "запрос не проходит проверку", method: http.MethodPost, path: "/shopping",
			body: `{"budget":0,"items":[]}`, user: buyer, want: http.StatusBadRequest,
		},
		{
			name: "история без токена", method: http.MethodGet, path: "/shopping",
			user: uuid.Nil, want: http.StatusUnauthorized,
		},
		{
			name: "прогон без токена", method: http.MethodGet, path: "/shopping/" + uuid.NewString(),
			user: uuid.Nil, want: http.StatusUnauthorized,
		},
		{
			name: "неразбираемый идентификатор прогона", method: http.MethodGet,
			path: "/shopping/не-uuid", user: buyer, want: http.StatusBadRequest,
		},
		{
			name: "неизвестный прогон", method: http.MethodGet,
			path: "/shopping/" + uuid.NewString(), user: buyer, want: http.StatusNotFound,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := do(handler, test.method, test.path, test.body, test.user)
			if recorder.Code != test.want {
				t.Errorf("код ответа %d, ожидался %d (%s)", recorder.Code, test.want, recorder.Body)
			}
		})
	}
}

func TestListHandlerErrors(t *testing.T) {
	env := newTestEnvironment(t, payment.Fee{}, nil)
	handler := registerHttpHandlers(env.gifts, env.shopaholic)

	tests := []struct {
		name string
		path string
		user uuid.UUID
		want int
	}{
		{"свой список без токена", "/wishlist/items", uuid.Nil, http.StatusUnauthorized},
		{"чужой список без токена", "/wishlist/" + uuid.NewString() + "/items", uuid.Nil, http.StatusUnauthorized},
		{"чужой список по неразбираемому пользователю", "/wishlist/не-uuid/items", uuid.New(), http.StatusBadRequest},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if code := do(handler, http.MethodGet, test.path, "", test.user).Code; code != test.want {
				t.Errorf("код ответа %d, ожидался %d", code, test.want)
			}
		})
	}
}

// TestForeignListHidesGivers фиксирует главное правило чужого списка:
// владелец не должен видеть, кто именно выбрал его подарок.
func TestForeignListHidesGivers(t *testing.T) {
	env := newTestEnvironment(t, payment.Fee{}, nil)
	handler := registerHttpHandlers(env.gifts, env.shopaholic)
	owner := uuid.New()
	giver := uuid.New()
	item := env.addProduct(t, owner)

	if code := do(handler, http.MethodPost,
		"/wishlist/items/"+item.Id.String()+"/reserve", "", giver).Code; code != http.StatusOK {
		t.Fatalf("резерв: код ответа %d", code)
	}

	recorder := do(handler, http.MethodGet, "/wishlist/items", "", owner)
	if recorder.Code != http.StatusOK {
		t.Fatalf("код ответа %d (%s)", recorder.Code, recorder.Body)
	}
	if strings.Contains(recorder.Body.String(), giver.String()) {
		t.Errorf("даритель виден владельцу списка: %s", recorder.Body)
	}
}
