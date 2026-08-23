package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wish/services"
	"wish/services/shared/account"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// fakeDatabase подменяет репозиторий: обработчики проверяются без базы,
// а поведение самого репозитория — интеграционным тестом.
type fakeDatabase struct {
	created   account.CreateAccount
	operator  *services.AuthorizedUser
	createId  uuid.UUID
	createErr error

	account *account.Account
	getErr  error
}

func (f *fakeDatabase) Create(_ context.Context, a account.CreateAccount, operator *services.AuthorizedUser) (uuid.UUID, error) {
	f.created = a
	f.operator = operator
	if f.createErr != nil {
		return uuid.Nil, f.createErr
	}
	return f.createId, nil
}

func (f *fakeDatabase) Get(context.Context, uuid.UUID) (*account.Account, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.account, nil
}

func (f *fakeDatabase) Close() error               { return nil }
func (f *fakeDatabase) Stats() sql.DBStats         { return sql.DBStats{} }
func (f *fakeDatabase) Ping(context.Context) error { return nil }

// authorized проставляет заголовок, который в рабочем контуре ставит шлюз
// после проверки токена: сервисы токен сами не разбирают.
func authorized(request *http.Request, user uuid.UUID, roles ...string) *http.Request {
	request.Header.Set("X-Authorized-Id", user.String())
	if len(roles) > 0 {
		request.Header.Set("X-Roles", strings.Join(roles, ","))
	}
	return request
}

func post(handler http.Handler, body string, request func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request(
		httptest.NewRequest(http.MethodPost, "/account", strings.NewReader(body))))
	return recorder
}

func TestCreateAccount(t *testing.T) {
	owner := uuid.New()
	created := uuid.New()
	db := &fakeDatabase{createId: created}
	handler := registerHttpHandlers(db)

	recorder := post(handler, `{"user_id":"`+owner.String()+`","type":"DEBIT"}`,
		func(r *http.Request) *http.Request { return authorized(r, owner) })

	if recorder.Code != http.StatusCreated {
		t.Fatalf("код ответа %d, ожидался %d (%s)", recorder.Code, http.StatusCreated, recorder.Body)
	}
	if got := recorder.Header().Get("X-Account-Id"); got != created.String() {
		t.Errorf("X-Account-Id %q, ожидался %q", got, created)
	}
	if db.created.UserId != owner || db.created.Type != account.TypeDebit {
		t.Errorf("в репозиторий ушло %+v", db.created)
	}
	if db.operator == nil || db.operator.Id != owner {
		t.Errorf("оператор не проброшен в репозиторий: %+v", db.operator)
	}
}

// TestCreateAccountForAnother фиксирует правило: счёт другому пользователю
// заводит только оператор.
func TestCreateAccountForAnother(t *testing.T) {
	owner := uuid.New()
	body := `{"user_id":"` + owner.String() + `","type":"DEBIT"}`

	t.Run("без роли оператора запрещено", func(t *testing.T) {
		recorder := post(registerHttpHandlers(&fakeDatabase{createId: uuid.New()}), body,
			func(r *http.Request) *http.Request { return authorized(r, uuid.New()) })
		if recorder.Code != http.StatusForbidden {
			t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusForbidden)
		}
	})

	t.Run("с ролью оператора разрешено", func(t *testing.T) {
		recorder := post(registerHttpHandlers(&fakeDatabase{createId: uuid.New()}), body,
			func(r *http.Request) *http.Request {
				return authorized(r, uuid.New(), services.RoleOperator)
			})
		if recorder.Code != http.StatusCreated {
			t.Errorf("код ответа %d, ожидался %d (%s)", recorder.Code, http.StatusCreated, recorder.Body)
		}
	})
}

func TestCreateAccountErrors(t *testing.T) {
	owner := uuid.New()
	valid := `{"user_id":"` + owner.String() + `","type":"DEBIT"}`

	tests := []struct {
		name    string
		body    string
		db      *fakeDatabase
		request func(*http.Request) *http.Request
		want    int
	}{
		{
			name:    "без заголовка авторизации",
			body:    valid,
			db:      &fakeDatabase{},
			request: func(r *http.Request) *http.Request { return r },
			want:    http.StatusUnauthorized,
		},
		{
			name: "нечитаемое тело",
			body: `{"user_id":`,
			db:   &fakeDatabase{},
			request: func(r *http.Request) *http.Request {
				return authorized(r, owner)
			},
			want: http.StatusBadRequest,
		},
		{
			name: "заявка не проходит проверку",
			body: `{"user_id":"` + owner.String() + `","type":"WHATEVER"}`,
			db:   &fakeDatabase{},
			request: func(r *http.Request) *http.Request {
				return authorized(r, owner)
			},
			want: http.StatusBadRequest,
		},
		{
			name: "счёт уже существует",
			body: valid,
			db:   &fakeDatabase{createErr: &pq.Error{Code: "23505"}},
			request: func(r *http.Request) *http.Request {
				return authorized(r, owner)
			},
			want: http.StatusConflict,
		},
		{
			name: "база недоступна",
			body: valid,
			db:   &fakeDatabase{createErr: errors.New("connection refused")},
			request: func(r *http.Request) *http.Request {
				return authorized(r, owner)
			},
			want: http.StatusInternalServerError,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := post(registerHttpHandlers(test.db), test.body, test.request)
			if recorder.Code != test.want {
				t.Errorf("код ответа %d, ожидался %d (%s)", recorder.Code, test.want, recorder.Body)
			}
		})
	}
}

func TestGetAccount(t *testing.T) {
	owner := uuid.New()
	id := uuid.New()
	started := time.Now().UTC().Truncate(time.Second)
	db := &fakeDatabase{account: &account.Account{
		Id: id, UserId: owner, Type: account.TypeDebit, State: "ACTIVE",
		Balance: 12345, StartedAt: &started,
	}}

	recorder := httptest.NewRecorder()
	registerHttpHandlers(db).ServeHTTP(recorder, authorized(
		httptest.NewRequest(http.MethodGet, "/account/"+id.String(), nil), owner))

	if recorder.Code != http.StatusOK {
		t.Fatalf("код ответа %d, ожидался %d (%s)", recorder.Code, http.StatusOK, recorder.Body)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type %q, ожидался application/json", got)
	}

	var loaded account.Account
	if err := json.Unmarshal(recorder.Body.Bytes(), &loaded); err != nil {
		t.Fatalf("разбор ответа: %v", err)
	}
	if loaded.Id != id || loaded.UserId != owner || loaded.Balance != 12345 {
		t.Errorf("получено %+v", loaded)
	}
}

func TestGetAccountErrors(t *testing.T) {
	owner := uuid.New()
	id := uuid.New()

	tests := []struct {
		name    string
		path    string
		db      *fakeDatabase
		request func(*http.Request) *http.Request
		want    int
	}{
		{
			name:    "без заголовка авторизации",
			path:    "/account/" + id.String(),
			db:      &fakeDatabase{},
			request: func(r *http.Request) *http.Request { return r },
			want:    http.StatusUnauthorized,
		},
		{
			name:    "идентификатор не разбирается",
			path:    "/account/не-uuid",
			db:      &fakeDatabase{},
			request: func(r *http.Request) *http.Request { return authorized(r, owner) },
			want:    http.StatusBadRequest,
		},
		{
			name:    "счёта нет",
			path:    "/account/" + id.String(),
			db:      &fakeDatabase{getErr: sql.ErrNoRows},
			request: func(r *http.Request) *http.Request { return authorized(r, owner) },
			want:    http.StatusNotFound,
		},
		{
			// Идентификатор счёта последовательный, поэтому чужой счёт
			// отдаётся как несуществующий, а не как запрещённый.
			name: "чужой счёт неотличим от отсутствующего",
			path: "/account/" + id.String(),
			db: &fakeDatabase{account: &account.Account{
				Id: id, UserId: uuid.New(), Type: account.TypeDebit,
			}},
			request: func(r *http.Request) *http.Request { return authorized(r, owner) },
			want:    http.StatusNotFound,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			registerHttpHandlers(test.db).ServeHTTP(recorder, test.request(
				httptest.NewRequest(http.MethodGet, test.path, nil)))
			if recorder.Code != test.want {
				t.Errorf("код ответа %d, ожидался %d (%s)", recorder.Code, test.want, recorder.Body)
			}
		})
	}
}

// TestGetForeignAccountAsOperator: оператору чужой счёт виден — иначе
// служебные сценарии работать не смогут.
func TestGetForeignAccountAsOperator(t *testing.T) {
	id := uuid.New()
	db := &fakeDatabase{account: &account.Account{Id: id, UserId: uuid.New(), Type: account.TypeDebit}}

	recorder := httptest.NewRecorder()
	registerHttpHandlers(db).ServeHTTP(recorder, authorized(
		httptest.NewRequest(http.MethodGet, "/account/"+id.String(), nil),
		uuid.New(), services.RoleOperator))

	if recorder.Code != http.StatusOK {
		t.Errorf("код ответа %d, ожидался %d (%s)", recorder.Code, http.StatusOK, recorder.Body)
	}
}
