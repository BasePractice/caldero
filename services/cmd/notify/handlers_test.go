package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wish/services/shared/notify"

	"github.com/google/uuid"
)

func newTestAPI(db Database, hub *Hub) http.Handler {
	return registerHttpHandlers(&api{db: db, hub: hub, codeTTL: 15 * time.Minute})
}

// authorized подставляет заголовки, которые проставляет шлюз после
// проверки токена: сервисы сами токен не разбирают.
func authorized(request *http.Request, user uuid.UUID, roles string) *http.Request {
	request.Header.Set("X-Authorized-Id", user.String())
	if roles != "" {
		request.Header.Set("X-Roles", roles)
	}
	return request
}

func TestPublishEvent(t *testing.T) {
	db := &fakeDatabase{}
	handler := newTestAPI(db, NewHub())
	user := uuid.New()

	body := `{"user_id":"` + user.String() + `","type":"WISHLIST_ITEM_RESERVED","payload":{"item":"Кофеварка"}}`
	request := authorized(httptest.NewRequest(http.MethodPost, "/notify/events", strings.NewReader(body)), user, "")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	// 202, а не 200: доставка асинхронна намеренно — отправка в мессенджер
	// не должна задерживать бизнес-операцию.
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("код ответа %d, ожидался %d (%s)", recorder.Code, http.StatusAccepted, recorder.Body)
	}
	if len(db.published) != 1 {
		t.Fatalf("опубликовано событий %d, ожидалось 1", len(db.published))
	}
	if recorder.Header().Get("X-Event-Id") == "" {
		t.Error("идентификатор события не возвращён")
	}
}

func TestPublishEventForAnotherUser(t *testing.T) {
	db := &fakeDatabase{}
	handler := newTestAPI(db, NewHub())
	target := uuid.New()

	body := `{"user_id":"` + target.String() + `","type":"WISHLIST_ITEM_RESERVED","payload":{"item":"Кофеварка"}}`

	t.Run("обычному пользователю запрещено", func(t *testing.T) {
		request := authorized(httptest.NewRequest(http.MethodPost, "/notify/events",
			strings.NewReader(body)), uuid.New(), "")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden {
			t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusForbidden)
		}
	})

	t.Run("оператору разрешено", func(t *testing.T) {
		request := authorized(httptest.NewRequest(http.MethodPost, "/notify/events",
			strings.NewReader(body)), uuid.New(), "operator")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusAccepted {
			t.Errorf("код ответа %d, ожидался %d (%s)", recorder.Code, http.StatusAccepted, recorder.Body)
		}
	})
}

func TestPublishRejectsUnknownType(t *testing.T) {
	handler := newTestAPI(&fakeDatabase{}, NewHub())
	user := uuid.New()

	body := `{"user_id":"` + user.String() + `","type":"WHATEVER"}`
	request := authorized(httptest.NewRequest(http.MethodPost, "/notify/events", strings.NewReader(body)), user, "")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusBadRequest)
	}
}

func TestMessagesWithoutWaiting(t *testing.T) {
	db := &fakeDatabase{}
	db.appendFeed(notify.Message{Seq: 1, Title: "Подарок"})
	handler := newTestAPI(db, NewHub())
	user := uuid.New()

	request := authorized(httptest.NewRequest(http.MethodGet, "/notify/messages?after=0", nil), user, "")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("код ответа %d", recorder.Code)
	}
	var response struct {
		Messages []notify.Message `json:"messages"`
		Cursor   int64            `json:"cursor"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("разбор ответа: %v", err)
	}
	if len(response.Messages) != 1 || response.Cursor != 1 {
		t.Errorf("ответ: %+v", response)
	}
}

// TestLongPollWakesOnMessage проверяет «пулинг как в вк»: запрос ждёт,
// пока сообщение не появится, и возвращает его без нового опроса.
func TestLongPollWakesOnMessage(t *testing.T) {
	db := &fakeDatabase{}
	hub := NewHub()
	handler := newTestAPI(db, hub)
	user := uuid.New()

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		request := authorized(httptest.NewRequest(http.MethodGet,
			"/notify/messages?after=0&wait=10", nil), user, "")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		done <- recorder
	}()

	// Пауза, чтобы запрос успел дойти до ожидания: без неё сообщение
	// оказалось бы в ленте раньше подписки, и тест проверял бы не то.
	time.Sleep(100 * time.Millisecond)
	message := notify.Message{Seq: 7, Title: "Котёл готов"}
	db.appendFeed(message)
	hub.Deliver(user, message)

	select {
	case recorder := <-done:
		var response struct {
			Messages []notify.Message `json:"messages"`
			Cursor   int64            `json:"cursor"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("разбор ответа: %v", err)
		}
		if len(response.Messages) != 1 || response.Messages[0].Seq != 7 {
			t.Errorf("ответ длинного опроса: %+v", response)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("длинный опрос не проснулся на новом сообщении")
	}
}

func TestLongPollReturnsOnTimeout(t *testing.T) {
	handler := newTestAPI(&fakeDatabase{}, NewHub())
	user := uuid.New()

	start := time.Now()
	request := authorized(httptest.NewRequest(http.MethodGet,
		"/notify/messages?after=5&wait=1", nil), user, "")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("код ответа %d", recorder.Code)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Errorf("запрос вернулся через %s, ожидалось ожидание в секунду", elapsed)
	}
	var response struct {
		Messages []notify.Message `json:"messages"`
		Cursor   int64            `json:"cursor"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("разбор ответа: %v", err)
	}
	// Курсор возвращается прежний: клиент повторит запрос с ним же.
	if len(response.Messages) != 0 || response.Cursor != 5 {
		t.Errorf("ответ по таймауту: %+v", response)
	}
}

func TestPreferences(t *testing.T) {
	db := &fakeDatabase{}
	handler := newTestAPI(db, NewHub())
	user := uuid.New()

	t.Run("чтение", func(t *testing.T) {
		request := authorized(httptest.NewRequest(http.MethodGet, "/notify/preferences", nil), user, "")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("код ответа %d", recorder.Code)
		}
	})

	t.Run("сохранение", func(t *testing.T) {
		body := `[{"type":"CALDRON_STATE_CHANGED","channel":"TELEGRAM","enabled":false}]`
		request := authorized(httptest.NewRequest(http.MethodPut, "/notify/preferences",
			strings.NewReader(body)), user, "")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("код ответа %d (%s)", recorder.Code, recorder.Body)
		}
		if len(db.saved) != 1 || db.saved[0].Enabled {
			t.Errorf("сохранено: %+v", db.saved)
		}
	})

	t.Run("неизвестный канал отклоняется", func(t *testing.T) {
		body := `[{"type":"CALDRON_STATE_CHANGED","channel":"SMOKE","enabled":true}]`
		request := authorized(httptest.NewRequest(http.MethodPut, "/notify/preferences",
			strings.NewReader(body)), user, "")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusBadRequest)
		}
	})
}

func TestUnauthorized(t *testing.T) {
	handler := newTestAPI(&fakeDatabase{}, NewHub())
	for _, path := range []string{"/notify/messages", "/notify/preferences", "/notify/telegram"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
			if recorder.Code != http.StatusUnauthorized {
				t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusUnauthorized)
			}
		})
	}
}

func TestTelegramLinkWithoutChannel(t *testing.T) {
	handler := newTestAPI(&fakeDatabase{}, NewHub())
	user := uuid.New()

	request := authorized(httptest.NewRequest(http.MethodPost, "/notify/telegram/link", nil), user, "")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	// Канал не настроен — честный отказ вместо кода, который никуда
	// не приведёт.
	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusServiceUnavailable)
	}
}
