package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wish/services/shared/payment"
	"wish/services/shared/wishlist"

	"github.com/google/uuid"
)

func authorized(request *http.Request, user uuid.UUID) *http.Request {
	// Заголовок проставляет шлюз после проверки токена: сервисы токен
	// сами не разбирают.
	request.Header.Set("X-Authorized-Id", user.String())
	return request
}

func TestAddItemHandler(t *testing.T) {
	env := newTestEnvironment(t, payment.Fee{}, nil)
	handler := registerHttpHandlers(env.gifts, env.shopaholic)
	owner := uuid.New()

	body := `{"kind":"PRODUCT","priority":1,"provider":"STUB","product_id":"coffee-machine"}`
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, authorized(
		httptest.NewRequest(http.MethodPost, "/wishlist/items", strings.NewReader(body)), owner))

	if recorder.Code != http.StatusCreated {
		t.Fatalf("код ответа %d, ожидался %d (%s)", recorder.Code, http.StatusCreated, recorder.Body)
	}
	var item wishlist.Item
	if err := json.Unmarshal(recorder.Body.Bytes(), &item); err != nil {
		t.Fatalf("разбор ответа: %v", err)
	}
	if item.Price <= 0 {
		t.Error("цена не взята из карточки площадки")
	}
	if recorder.Header().Get("X-Item-Id") == "" {
		t.Error("идентификатор элемента не возвращён")
	}
}

func TestAddItemValidation(t *testing.T) {
	env := newTestEnvironment(t, payment.Fee{}, nil)
	handler := registerHttpHandlers(env.gifts, env.shopaholic)

	// Цена и название от клиента не принимаются: они берутся из карточки.
	body := `{"kind":"PRODUCT","priority":1,"provider":"STUB","product_id":"x","price":1}`
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, authorized(
		httptest.NewRequest(http.MethodPost, "/wishlist/items", strings.NewReader(body)), uuid.New()))

	if recorder.Code != http.StatusBadRequest {
		t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusBadRequest)
	}
}

func TestActionStatusCodes(t *testing.T) {
	ctx := context.Background()
	env := newTestEnvironment(t, payment.Fee{}, nil)
	handler := registerHttpHandlers(env.gifts, env.shopaholic)
	owner := uuid.New()
	giver := uuid.New()
	item := env.addProduct(t, owner)

	do := func(user uuid.UUID, path string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, authorized(httptest.NewRequest(http.MethodPost, path, nil), user))
		return recorder
	}

	t.Run("резерв своего элемента запрещён", func(t *testing.T) {
		if code := do(owner, "/wishlist/items/"+item.Id.String()+"/reserve").Code; code != http.StatusForbidden {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusForbidden)
		}
	})

	t.Run("резерв дарителем", func(t *testing.T) {
		if code := do(giver, "/wishlist/items/"+item.Id.String()+"/reserve").Code; code != http.StatusOK {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusOK)
		}
	})

	t.Run("повторный резерв другим дарителем — 404", func(t *testing.T) {
		// Выбранный подарок для других дарителей не существует.
		if code := do(uuid.New(), "/wishlist/items/"+item.Id.String()+"/reserve").Code; code != http.StatusNotFound {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusNotFound)
		}
	})

	t.Run("акцепт до подтверждения — 409", func(t *testing.T) {
		if code := do(giver, "/wishlist/items/"+item.Id.String()+"/accept").Code; code != http.StatusConflict {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusConflict)
		}
	})

	t.Run("подтверждение владельцем", func(t *testing.T) {
		if code := do(owner, "/wishlist/items/"+item.Id.String()+"/confirm").Code; code != http.StatusOK {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusOK)
		}
	})

	t.Run("акцепт чужим дарителем — 403", func(t *testing.T) {
		if code := do(uuid.New(), "/wishlist/items/"+item.Id.String()+"/accept").Code; code != http.StatusForbidden {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusForbidden)
		}
	})

	t.Run("несуществующий элемент — 404", func(t *testing.T) {
		if code := do(owner, "/wishlist/items/"+uuid.New().String()+"/confirm").Code; code != http.StatusNotFound {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusNotFound)
		}
	})

	if _, err := env.gifts.Accept(ctx, giver, item.Id); err != nil {
		t.Fatalf("акцепт: %v", err)
	}
}

func TestMoneyGiftWithoutWallet(t *testing.T) {
	ctx := context.Background()
	env := newTestEnvironment(t, payment.Fee{}, nil)
	env.gifts.wallet = nil
	handler := registerHttpHandlers(env.gifts, env.shopaholic)
	owner := uuid.New()
	giver := uuid.New()

	item, err := env.gifts.Add(ctx, owner, wishlist.CreateItem{
		Kind: wishlist.KindMoney, Priority: 1, Amount: 1_000_00, Title: "На велосипед",
	})
	if err != nil {
		t.Fatalf("добавление элемента: %v", err)
	}
	if _, err = env.gifts.Reserve(ctx, giver, item.Id); err != nil {
		t.Fatalf("резервирование: %v", err)
	}
	if _, err = env.gifts.Confirm(ctx, owner, item.Id); err != nil {
		t.Fatalf("подтверждение: %v", err)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, authorized(
		httptest.NewRequest(http.MethodPost, "/wishlist/items/"+item.Id.String()+"/accept", nil), giver))

	// Кошелёк не настроен — это отказ зависимости, а не ошибка клиента.
	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if recorder.Header().Get("Retry-After") == "" {
		t.Error("нет заголовка Retry-After при отказе зависимости")
	}
}

func TestForeignListHandler(t *testing.T) {
	ctx := context.Background()
	env := newTestEnvironment(t, payment.Fee{}, nil)
	handler := registerHttpHandlers(env.gifts, env.shopaholic)
	owner := uuid.New()
	giver := uuid.New()
	item := env.addProduct(t, owner)
	if _, err := env.gifts.Reserve(ctx, giver, item.Id); err != nil {
		t.Fatalf("резервирование: %v", err)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, authorized(
		httptest.NewRequest(http.MethodGet, "/wishlist/"+owner.String()+"/items", nil), uuid.New()))
	if recorder.Code != http.StatusOK {
		t.Fatalf("код ответа %d", recorder.Code)
	}

	var items []wishlist.Item
	if err := json.Unmarshal(recorder.Body.Bytes(), &items); err != nil {
		t.Fatalf("разбор ответа: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("выбранный подарок виден другому дарителю: %+v", items)
	}
}

func TestUnauthorized(t *testing.T) {
	env := newTestEnvironment(t, payment.Fee{}, nil)
	handler := registerHttpHandlers(env.gifts, env.shopaholic)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/wishlist/items", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusUnauthorized)
	}
}
