package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wish/services/shared/caldron"

	"github.com/google/uuid"
)

func authorized(request *http.Request, user uuid.UUID) *http.Request {
	// Заголовок проставляет шлюз после проверки токена: сервисы токен
	// сами не разбирают.
	request.Header.Set("X-Authorized-Id", user.String())
	return request
}

func TestCreateCaldronHandler(t *testing.T) {
	env := newEnvironment(t)
	handler := registerHttpHandlers(env.caldrons)
	creator := uuid.New()

	body := `{"title":"Юбилей","type":"GIFT","mode":"FIXED","creator_participates":true,"amount":250000}`
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, authorized(
		httptest.NewRequest(http.MethodPost, "/caldrons", strings.NewReader(body)), creator))

	if recorder.Code != http.StatusCreated {
		t.Fatalf("код ответа %d, ожидался %d (%s)", recorder.Code, http.StatusCreated, recorder.Body)
	}
	var pot caldron.Caldron
	if err := json.Unmarshal(recorder.Body.Bytes(), &pot); err != nil {
		t.Fatalf("разбор ответа: %v", err)
	}
	if pot.State != caldron.StatePreparing {
		t.Errorf("состояние %s, ожидалось %s", pot.State, caldron.StatePreparing)
	}
	if !pot.IsParticipant(creator) {
		t.Error("создатель-участник не попал в список")
	}
	if recorder.Header().Get("X-Caldron-Id") == "" {
		t.Error("идентификатор котла не возвращён")
	}
}

func TestCreateCaldronValidation(t *testing.T) {
	env := newEnvironment(t)
	handler := registerHttpHandlers(env.caldrons)

	// Точная сумма без суммы: правило взноса не определено.
	body := `{"title":"Юбилей","type":"GIFT","mode":"FIXED"}`
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, authorized(
		httptest.NewRequest(http.MethodPost, "/caldrons", strings.NewReader(body)), uuid.New()))

	if recorder.Code != http.StatusBadRequest {
		t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusBadRequest)
	}
}

func TestCaldronStatusCodes(t *testing.T) {
	env := newEnvironment(t)
	handler := registerHttpHandlers(env.caldrons)
	creator := uuid.New()
	member := uuid.New()
	stranger := uuid.New()
	env.wallet.fund(creator, 10_000_00)
	env.wallet.fund(member, 10_000_00)
	pot := env.fixedCaldron(t, creator, 2_500_00, member)

	post := func(user uuid.UUID, path, body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, authorized(
			httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)), user))
		return recorder
	}
	path := "/caldrons/" + pot.Id.String()

	t.Run("посторонний не видит котла", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, authorized(httptest.NewRequest(http.MethodGet, path, nil), stranger))
		if recorder.Code != http.StatusNotFound {
			t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusNotFound)
		}
	})

	t.Run("участник не добавляет других", func(t *testing.T) {
		body := fmt.Sprintf(`{"user_id":%q}`, uuid.New())
		if code := post(member, path+"/participants", body).Code; code != http.StatusForbidden {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusForbidden)
		}
	})

	t.Run("несобранный котёл не завершается", func(t *testing.T) {
		body := fmt.Sprintf(`{"winner":%q}`, member)
		if code := post(creator, path+"/settle", body).Code; code != http.StatusConflict {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusConflict)
		}
	})

	t.Run("взнос", func(t *testing.T) {
		if code := post(creator, path+"/contribute", "").Code; code != http.StatusOK {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusOK)
		}
		if code := post(member, path+"/contribute", "").Code; code != http.StatusOK {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusOK)
		}
	})

	t.Run("повторный взнос — 409", func(t *testing.T) {
		if code := post(member, path+"/contribute", "").Code; code != http.StatusConflict {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusConflict)
		}
	})

	t.Run("посторонний не вносит", func(t *testing.T) {
		if code := post(stranger, path+"/contribute", "").Code; code != http.StatusNotFound {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusNotFound)
		}
	})

	t.Run("победитель обязан быть участником", func(t *testing.T) {
		body := fmt.Sprintf(`{"winner":%q}`, uuid.New())
		if code := post(creator, path+"/settle", body).Code; code != http.StatusForbidden {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusForbidden)
		}
	})

	t.Run("завершение котла", func(t *testing.T) {
		body := fmt.Sprintf(`{"winner":%q}`, member)
		recorder := post(creator, path+"/settle", body)
		if recorder.Code != http.StatusOK {
			t.Fatalf("код ответа %d (%s)", recorder.Code, recorder.Body)
		}
		if env.wallet.balanceOf(member) != 12_500_00 {
			t.Errorf("победителю досталось %s", env.wallet.balanceOf(member))
		}
	})

	t.Run("завершённый котёл не отменяется", func(t *testing.T) {
		if code := post(creator, path+"/cancel", "").Code; code != http.StatusConflict {
			t.Errorf("код ответа %d, ожидался %d", code, http.StatusConflict)
		}
	})
}

func TestContributionOutOfRange(t *testing.T) {
	env := newEnvironment(t)
	handler := registerHttpHandlers(env.caldrons)
	creator := uuid.New()
	env.wallet.fund(creator, 100_000_00)

	pot, err := env.caldrons.Create(t.Context(), creator, caldron.CreateCaldron{
		Title: "Юбилей", Type: caldron.TypeLuck, Mode: caldron.ModeRange,
		CreatorParticipates: true, MinAmount: 1_000_00, MaxAmount: 5_000_00,
	})
	if err != nil {
		t.Fatalf("создание котла: %v", err)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, authorized(httptest.NewRequest(http.MethodPost,
		"/caldrons/"+pot.Id.String()+"/contribute", strings.NewReader(`{"amount":900000}`)), creator))

	// Сумма вне диапазона — ошибка в запросе, а не конфликт состояния.
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusBadRequest)
	}
}

func TestUnauthorized(t *testing.T) {
	env := newEnvironment(t)
	handler := registerHttpHandlers(env.caldrons)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/caldrons", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusUnauthorized)
	}
}
