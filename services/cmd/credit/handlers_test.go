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

	wallet "wish/middleware/wallet/v1"
	"wish/services"
	"wish/services/shared/credit"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// fakeDatabase подменяет репозиторий: обработчики проверяются без базы,
// а поведение самого репозитория — интеграционным тестом.
type fakeDatabase struct {
	credit    *credit.Credit
	getErr    error
	createId  uuid.UUID
	createErr error
	created   credit.CreateCredit
	operator  *services.AuthorizedUser

	payment    PaymentRecord
	paymentErr error
}

func (f *fakeDatabase) Create(_ context.Context, c credit.CreateCredit, operator *services.AuthorizedUser) (uuid.UUID, error) {
	f.created, f.operator = c, operator
	if f.createErr != nil {
		return uuid.Nil, f.createErr
	}
	return f.createId, nil
}

func (f *fakeDatabase) Get(context.Context, uuid.UUID) (*credit.Credit, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.credit, nil
}

func (f *fakeDatabase) RecordPayment(_ context.Context, payment PaymentRecord) error {
	f.payment = payment
	return f.paymentErr
}

func (f *fakeDatabase) Close() error               { return nil }
func (f *fakeDatabase) Stats() sql.DBStats         { return sql.DBStats{} }
func (f *fakeDatabase) Ping(context.Context) error { return nil }

// fakeWallet подменяет клиента кошелька: сервису кредитов от него нужно
// только списание.
type fakeWallet struct {
	err     error
	request *wallet.OperationRequest
	ctx     context.Context
}

func (f *fakeWallet) Credit(ctx context.Context, request *wallet.OperationRequest, _ ...grpc.CallOption) (*wallet.TransactionReply, error) {
	f.ctx, f.request = ctx, request
	if f.err != nil {
		return nil, f.err
	}
	return &wallet.TransactionReply{}, nil
}

// authorized проставляет заголовок, который в рабочем контуре ставит шлюз
// после проверки токена.
func authorized(request *http.Request, user uuid.UUID, roles ...string) *http.Request {
	request.Header.Set("X-Authorized-Id", user.String())
	if len(roles) > 0 {
		request.Header.Set("X-Roles", strings.Join(roles, ","))
	}
	return request
}

func borrowerCredit(borrower, creator uuid.UUID) *credit.Credit {
	return &credit.Credit{
		UserId: borrower, CreatorId: creator,
		Type: credit.TypeSimple, Kind: credit.KindAnnuity,
		Month: 12, Rate: 1250, Balance: 100_000, AlreadyPaid: 10_000,
	}
}

func TestCreateCreditHandler(t *testing.T) {
	borrower := uuid.New()
	created := uuid.New()
	db := &fakeDatabase{createId: created}

	body := `{"user_id":"` + borrower.String() + `","type":"SIMPLE","kind":"ANN",` +
		`"month":12,"rate_bp":1250,"balance":100000,"already_paid":0}`
	recorder := httptest.NewRecorder()
	registerHttpHandlers(db, &fakeWallet{}).ServeHTTP(recorder, authorized(
		httptest.NewRequest(http.MethodPost, "/credit", strings.NewReader(body)), borrower))

	if recorder.Code != http.StatusCreated {
		t.Fatalf("код ответа %d, ожидался %d (%s)", recorder.Code, http.StatusCreated, recorder.Body)
	}
	if got := recorder.Header().Get("X-Credit-Id"); got != created.String() {
		t.Errorf("X-Credit-Id %q, ожидался %q", got, created)
	}
	// Выдавший кредит фиксируется отдельно от заёмщика: схема заложена
	// под сценарий «оператор выдаёт кредит клиенту».
	if db.operator == nil || db.operator.Id != borrower {
		t.Errorf("оператор не проброшен в репозиторий: %+v", db.operator)
	}
}

func TestCreateCreditHandlerErrors(t *testing.T) {
	borrower := uuid.New()
	valid := `{"user_id":"` + borrower.String() + `","type":"SIMPLE","kind":"ANN",` +
		`"month":12,"rate_bp":1250,"balance":100000,"already_paid":0}`

	tests := []struct {
		name  string
		body  string
		db    *fakeDatabase
		user  uuid.UUID
		roles []string
		want  int
	}{
		{
			name: "нечитаемое тело",
			body: `{"user_id":`,
			db:   &fakeDatabase{},
			user: borrower,
			want: http.StatusBadRequest,
		},
		{
			name: "заявка не проходит проверку",
			body: `{"user_id":"` + borrower.String() + `","type":"WHATEVER","kind":"ANN",` +
				`"month":12,"rate_bp":1250,"balance":100000,"already_paid":0}`,
			db:   &fakeDatabase{},
			user: borrower,
			want: http.StatusBadRequest,
		},
		{
			// Раньше любой пользователь мог оформить кредит на любого
			// другого: роли оператора в системе не было.
			name: "кредит другому без роли оператора",
			body: valid,
			db:   &fakeDatabase{createId: uuid.New()},
			user: uuid.New(),
			want: http.StatusForbidden,
		},
		{
			name:  "кредит другому с ролью оператора",
			body:  valid,
			db:    &fakeDatabase{createId: uuid.New()},
			user:  uuid.New(),
			roles: []string{services.RoleOperator},
			want:  http.StatusCreated,
		},
		{
			name: "такой кредит уже есть",
			body: valid,
			db:   &fakeDatabase{createErr: &pq.Error{Code: "23505"}},
			user: borrower,
			want: http.StatusConflict,
		},
		{
			name: "база недоступна",
			body: valid,
			db:   &fakeDatabase{createErr: errors.New("connection refused")},
			user: borrower,
			want: http.StatusInternalServerError,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			registerHttpHandlers(test.db, &fakeWallet{}).ServeHTTP(recorder, authorized(
				httptest.NewRequest(http.MethodPost, "/credit", strings.NewReader(test.body)),
				test.user, test.roles...))
			if recorder.Code != test.want {
				t.Errorf("код ответа %d, ожидался %d (%s)", recorder.Code, test.want, recorder.Body)
			}
		})
	}
}

func TestCreateCreditUnauthorized(t *testing.T) {
	recorder := httptest.NewRecorder()
	registerHttpHandlers(&fakeDatabase{}, &fakeWallet{}).ServeHTTP(recorder,
		httptest.NewRequest(http.MethodPost, "/credit", strings.NewReader(`{}`)))

	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusUnauthorized)
	}
}

func TestScheduleHandler(t *testing.T) {
	borrower := uuid.New()
	creator := uuid.New()
	id := uuid.New()
	db := &fakeDatabase{credit: borrowerCredit(borrower, creator)}

	recorder := httptest.NewRecorder()
	registerHttpHandlers(db, &fakeWallet{}).ServeHTTP(recorder, authorized(
		httptest.NewRequest(http.MethodGet, "/credits/"+id.String()+"/schedule", nil), borrower))

	if recorder.Code != http.StatusOK {
		t.Fatalf("код ответа %d, ожидался %d (%s)", recorder.Code, http.StatusOK, recorder.Body)
	}
	if got := recorder.Header().Get("X-Credit-Id"); got != id.String() {
		t.Errorf("X-Credit-Id %q, ожидался %q", got, id)
	}

	var payments []credit.Payment
	if err := json.Unmarshal(recorder.Body.Bytes(), &payments); err != nil {
		t.Fatalf("разбор ответа: %v", err)
	}
	if len(payments) != 12 {
		t.Errorf("платежей %d, ожидалось 12", len(payments))
	}
}

// TestScheduleHandlerAccess фиксирует правило доступа: график виден
// заёмщику, выдавшему кредит и оператору. Чужой кредит отдаётся как
// несуществующий — 403 подтвердил бы, что он есть.
func TestScheduleHandlerAccess(t *testing.T) {
	borrower := uuid.New()
	creator := uuid.New()
	id := uuid.New()

	tests := []struct {
		name  string
		user  uuid.UUID
		roles []string
		want  int
	}{
		{"заёмщик", borrower, nil, http.StatusOK},
		{"выдавший кредит", creator, nil, http.StatusOK},
		{"оператор", uuid.New(), []string{services.RoleOperator}, http.StatusOK},
		{"посторонний", uuid.New(), nil, http.StatusNotFound},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := &fakeDatabase{credit: borrowerCredit(borrower, creator)}
			recorder := httptest.NewRecorder()
			registerHttpHandlers(db, &fakeWallet{}).ServeHTTP(recorder, authorized(
				httptest.NewRequest(http.MethodGet, "/credits/"+id.String()+"/schedule", nil),
				test.user, test.roles...))
			if recorder.Code != test.want {
				t.Errorf("код ответа %d, ожидался %d (%s)", recorder.Code, test.want, recorder.Body)
			}
		})
	}
}

func TestScheduleHandlerErrors(t *testing.T) {
	borrower := uuid.New()
	id := uuid.New()

	tests := []struct {
		name       string
		path       string
		db         *fakeDatabase
		authorized bool
		want       int
	}{
		{
			name: "без заголовка авторизации",
			path: "/credits/" + id.String() + "/schedule",
			db:   &fakeDatabase{},
			want: http.StatusUnauthorized,
		},
		{
			name:       "идентификатор не разбирается",
			path:       "/credits/не-uuid/schedule",
			db:         &fakeDatabase{},
			authorized: true,
			want:       http.StatusBadRequest,
		},
		{
			name:       "кредита нет",
			path:       "/credits/" + id.String() + "/schedule",
			db:         &fakeDatabase{getErr: ErrCreditNotFound},
			authorized: true,
			want:       http.StatusNotFound,
		},
		{
			// Отсутствие записи отличается от сбоя базы: раньше обработчик
			// отвечал 404 на любую ошибку, включая недоступную базу.
			name:       "база недоступна",
			path:       "/credits/" + id.String() + "/schedule",
			db:         &fakeDatabase{getErr: errors.New("connection refused")},
			authorized: true,
			want:       http.StatusInternalServerError,
		},
		{
			name: "график посчитать нельзя",
			path: "/credits/" + id.String() + "/schedule",
			db: &fakeDatabase{credit: &credit.Credit{
				UserId: borrower, Type: credit.TypeSimple, Kind: "WHATEVER",
				Month: 12, Rate: 1250, Balance: 100_000,
			}},
			authorized: true,
			want:       http.StatusUnprocessableEntity,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			if test.authorized {
				request = authorized(request, borrower)
			}
			recorder := httptest.NewRecorder()
			registerHttpHandlers(test.db, &fakeWallet{}).ServeHTTP(recorder, request)
			if recorder.Code != test.want {
				t.Errorf("код ответа %d, ожидался %d (%s)", recorder.Code, test.want, recorder.Body)
			}
		})
	}
}

func TestPayCreditHandler(t *testing.T) {
	borrower := uuid.New()
	id := uuid.New()
	db := &fakeDatabase{credit: borrowerCredit(borrower, uuid.New())}
	walletClient := &fakeWallet{}

	body := `{"idempotency_key":"pay-1","amount":5000}`
	recorder := httptest.NewRecorder()
	registerHttpHandlers(db, walletClient).ServeHTTP(recorder, authorized(
		httptest.NewRequest(http.MethodPost, "/credits/"+id.String()+"/payments",
			strings.NewReader(body)), borrower))

	if recorder.Code != http.StatusCreated {
		t.Fatalf("код ответа %d, ожидался %d (%s)", recorder.Code, http.StatusCreated, recorder.Body)
	}

	// Ключ идемпотентности доходит и до кошелька, и до записи платежа:
	// повтор запроса не должен ни списать средства дважды, ни записать
	// второй платёж.
	if walletClient.request.GetIdempotencyKey() != "pay-1" {
		t.Errorf("ключ в кошельке %q, ожидался pay-1", walletClient.request.GetIdempotencyKey())
	}
	if walletClient.request.GetValue() != 5000 {
		t.Errorf("сумма списания %d, ожидалось 5000", walletClient.request.GetValue())
	}
	if db.payment.IdempotencyKey != "pay-1" || db.payment.Amount != 5000 {
		t.Errorf("записан платёж %+v", db.payment)
	}

	// Списание идёт от имени заёмщика: кошелёк проверяет, у кого списывают.
	md, ok := metadata.FromOutgoingContext(walletClient.ctx)
	if !ok {
		t.Fatal("вызов кошелька ушёл без авторизации")
	}
	if values := md.Get("x-authorized-id"); len(values) != 1 || values[0] != borrower.String() {
		t.Errorf("списание от имени %v, ожидался заёмщик %s", values, borrower)
	}
}

// TestPayCreditRepeat: повтор запроса должен давать тот же ответ, что
// и первый — кошелёк по тому же ключу средства второй раз не спишет,
// а платёж уже записан.
func TestPayCreditRepeat(t *testing.T) {
	borrower := uuid.New()
	id := uuid.New()
	db := &fakeDatabase{
		credit:     borrowerCredit(borrower, uuid.New()),
		paymentErr: ErrPaymentAlreadyRecorded,
	}

	recorder := httptest.NewRecorder()
	registerHttpHandlers(db, &fakeWallet{}).ServeHTTP(recorder, authorized(
		httptest.NewRequest(http.MethodPost, "/credits/"+id.String()+"/payments",
			strings.NewReader(`{"idempotency_key":"pay-1","amount":5000}`)), borrower))

	if recorder.Code != http.StatusCreated {
		t.Errorf("код ответа %d, ожидался %d (%s)", recorder.Code, http.StatusCreated, recorder.Body)
	}
}

func TestPayCreditErrors(t *testing.T) {
	borrower := uuid.New()
	id := uuid.New()
	valid := `{"idempotency_key":"pay-1","amount":5000}`

	tests := []struct {
		name       string
		path       string
		body       string
		db         *fakeDatabase
		wallet     *fakeWallet
		user       uuid.UUID
		authorized bool
		want       int
	}{
		{
			name: "без заголовка авторизации",
			path: "/credits/" + id.String() + "/payments",
			body: valid,
			db:   &fakeDatabase{},
			want: http.StatusUnauthorized,
		},
		{
			name:       "идентификатор не разбирается",
			path:       "/credits/не-uuid/payments",
			body:       valid,
			db:         &fakeDatabase{},
			authorized: true,
			want:       http.StatusBadRequest,
		},
		{
			name:       "нечитаемое тело",
			path:       "/credits/" + id.String() + "/payments",
			body:       `{"amount":`,
			db:         &fakeDatabase{},
			authorized: true,
			want:       http.StatusBadRequest,
		},
		{
			name:       "без ключа идемпотентности",
			path:       "/credits/" + id.String() + "/payments",
			body:       `{"amount":5000}`,
			db:         &fakeDatabase{},
			authorized: true,
			want:       http.StatusBadRequest,
		},
		{
			name:       "неположительная сумма",
			path:       "/credits/" + id.String() + "/payments",
			body:       `{"idempotency_key":"pay-1","amount":0}`,
			db:         &fakeDatabase{},
			authorized: true,
			want:       http.StatusBadRequest,
		},
		{
			name:       "кредита нет",
			path:       "/credits/" + id.String() + "/payments",
			body:       valid,
			db:         &fakeDatabase{getErr: ErrCreditNotFound},
			authorized: true,
			want:       http.StatusNotFound,
		},
		{
			name:       "база недоступна",
			path:       "/credits/" + id.String() + "/payments",
			body:       valid,
			db:         &fakeDatabase{getErr: errors.New("connection refused")},
			authorized: true,
			want:       http.StatusInternalServerError,
		},
		{
			// Оператор выдаёт кредит, но не гасит его чужими средствами.
			name:       "платёж по чужому кредиту",
			path:       "/credits/" + id.String() + "/payments",
			body:       valid,
			db:         &fakeDatabase{credit: borrowerCredit(uuid.New(), uuid.New())},
			authorized: true,
			want:       http.StatusNotFound,
		},
		{
			name:       "сумма больше остатка долга",
			path:       "/credits/" + id.String() + "/payments",
			body:       `{"idempotency_key":"pay-1","amount":100000}`,
			db:         &fakeDatabase{credit: borrowerCredit(borrower, uuid.New())},
			authorized: true,
			want:       http.StatusBadRequest,
		},
		{
			name:       "на кошельке не хватает средств",
			path:       "/credits/" + id.String() + "/payments",
			body:       valid,
			db:         &fakeDatabase{credit: borrowerCredit(borrower, uuid.New())},
			wallet:     &fakeWallet{err: status.Error(codes.FailedPrecondition, "insufficient funds")},
			authorized: true,
			want:       http.StatusPaymentRequired,
		},
		{
			name:       "кошелёк недоступен",
			path:       "/credits/" + id.String() + "/payments",
			body:       valid,
			db:         &fakeDatabase{credit: borrowerCredit(borrower, uuid.New())},
			wallet:     &fakeWallet{err: status.Error(codes.Unavailable, "connection refused")},
			authorized: true,
			want:       http.StatusBadGateway,
		},
		{
			// Средства уже списаны, а платёж не записан: повтор с тем же
			// ключом — единственный способ довести операцию до конца.
			name:       "платёж списан, но не записан",
			path:       "/credits/" + id.String() + "/payments",
			body:       valid,
			db:         &fakeDatabase{credit: borrowerCredit(borrower, uuid.New()), paymentErr: errors.New("сбой записи")},
			authorized: true,
			want:       http.StatusInternalServerError,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			if test.authorized {
				request = authorized(request, borrower)
			}
			walletClient := test.wallet
			if walletClient == nil {
				walletClient = &fakeWallet{}
			}

			recorder := httptest.NewRecorder()
			registerHttpHandlers(test.db, walletClient).ServeHTTP(recorder, request)
			if recorder.Code != test.want {
				t.Errorf("код ответа %d, ожидался %d (%s)", recorder.Code, test.want, recorder.Body)
			}
		})
	}
}

// TestWalletCreditErrors проверяет перевод кодов gRPC в ошибки домена:
// обработчику незачем знать про транспорт, а клиенту нужна причина,
// а не «Internal».
func TestWalletCreditErrors(t *testing.T) {
	operator := &services.AuthorizedUser{Id: uuid.New()}

	t.Run("кошелёк не настроен", func(t *testing.T) {
		err := walletCredit(context.Background(), nil, operator, "key", 100, "сообщение")
		if err == nil {
			t.Fatal("ненастроенный кошелёк не превратился в ошибку")
		}
	})

	t.Run("не хватает средств", func(t *testing.T) {
		err := walletCredit(context.Background(),
			&fakeWallet{err: status.Error(codes.FailedPrecondition, "not enough")},
			operator, "key", 100, "сообщение")
		if !errors.Is(err, ErrInsufficientFunds) {
			t.Errorf("ошибка %v, ожидалась ErrInsufficientFunds", err)
		}
	})

	t.Run("кошелька нет", func(t *testing.T) {
		err := walletCredit(context.Background(),
			&fakeWallet{err: status.Error(codes.NotFound, "no wallet")},
			operator, "key", 100, "сообщение")
		if err == nil || !strings.Contains(err.Error(), "wallet not found") {
			t.Errorf("ошибка %v не называет причину", err)
		}
	})

	t.Run("прочий отказ", func(t *testing.T) {
		err := walletCredit(context.Background(),
			&fakeWallet{err: status.Error(codes.Internal, "boom")},
			operator, "key", 100, "сообщение")
		if err == nil || !strings.Contains(err.Error(), "debiting wallet") {
			t.Errorf("ошибка %v не называет причину", err)
		}
	})
}
