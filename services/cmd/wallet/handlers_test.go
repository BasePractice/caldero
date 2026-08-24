package main

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	wallet "wish/middleware/wallet/v1"
	"wish/services"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fakeDatabase подменяет репозиторий: обработчики проверяются без базы,
// а сами операции над деньгами — интеграционным тестом, потому что их
// корректность держится на транзакциях и блокировках строк.
type fakeDatabase struct {
	transaction Transaction
	err         error

	wallets []*wallet.InformationReply
	// owner запоминает, от чьего имени пришёл вызов: чужой кошелёк
	// доступен только оператору, и обработчик обязан передавать
	// проверенного пользователя, а не то, что прислал клиент.
	owner  uuid.UUID
	params any

	history       []Transaction
	historyLimit  int
	historyBefore *time.Time

	stateChanged string
}

func (f *fakeDatabase) Information(_ context.Context, userId uuid.UUID, cb func(*wallet.InformationReply)) error {
	f.owner = userId
	if f.err != nil {
		return f.err
	}
	for _, reply := range f.wallets {
		cb(reply)
	}
	return nil
}

func (f *fakeDatabase) Debit(_ context.Context, owner uuid.UUID, params OperationParams) (Transaction, error) {
	f.owner, f.params = owner, params
	return f.transaction, f.err
}

func (f *fakeDatabase) Credit(_ context.Context, owner uuid.UUID, params OperationParams) (Transaction, error) {
	f.owner, f.params = owner, params
	return f.transaction, f.err
}

func (f *fakeDatabase) Transfer(_ context.Context, owner uuid.UUID, params TransferParams) (Transaction, error) {
	f.owner, f.params = owner, params
	return f.transaction, f.err
}

func (f *fakeDatabase) Reserve(_ context.Context, owner uuid.UUID, params ReserveParams) (Transaction, error) {
	f.owner, f.params = owner, params
	return f.transaction, f.err
}

func (f *fakeDatabase) Confirm(_ context.Context, owner, transactionId uuid.UUID) (Transaction, error) {
	f.owner, f.params = owner, transactionId
	return f.transaction, f.err
}

func (f *fakeDatabase) Reject(_ context.Context, owner, transactionId uuid.UUID) (Transaction, error) {
	f.owner, f.params = owner, transactionId
	return f.transaction, f.err
}

func (f *fakeDatabase) ReleaseExpiredReservations(context.Context) (int64, error) {
	return 0, f.err
}

func (f *fakeDatabase) History(_ context.Context, owner, _ uuid.UUID, limit int, before *time.Time) ([]Transaction, error) {
	f.owner, f.historyLimit, f.historyBefore = owner, limit, before
	return f.history, f.err
}

func (f *fakeDatabase) ChangeState(_ context.Context, owner, _ uuid.UUID, state string) error {
	f.owner, f.stateChanged = owner, state
	return f.err
}

func (f *fakeDatabase) EnsurePartitions(context.Context, int) (int, error)    { return 0, f.err }
func (f *fakeDatabase) DefaultPartitionRows(context.Context) (int64, error)   { return 0, f.err }
func (f *fakeDatabase) DetachOldPartitions(context.Context, int) (int, error) { return 0, f.err }
func (f *fakeDatabase) OldestPartition(context.Context) (time.Time, error) {
	return time.Time{}, f.err
}
func (f *fakeDatabase) Close() error               { return nil }
func (f *fakeDatabase) Stats() sql.DBStats         { return sql.DBStats{} }
func (f *fakeDatabase) Ping(context.Context) error { return nil }

// callerContext собирает контекст так, как его оставляет перехватчик
// авторизации. Ключ контекста приватен для пакета services, поэтому
// контекст строится тем же перехватчиком, а не подделкой ключа.
func callerContext(t *testing.T, id uuid.UUID, roles ...string) context.Context {
	t.Helper()
	pairs := []string{"x-authorized-id", id.String()}
	for _, role := range roles {
		pairs = append(pairs, "x-roles", role)
	}

	var authorized context.Context
	_, err := services.AuthorizeUnaryInterceptor()(
		metadata.NewIncomingContext(context.Background(), metadata.Pairs(pairs...)), nil,
		&grpc.UnaryServerInfo{FullMethod: "/test"},
		func(ctx context.Context, _ any) (any, error) {
			authorized = ctx
			return nil, nil
		})
	if err != nil {
		t.Fatalf("авторизация вызова: %v", err)
	}
	return authorized
}

func transaction(walletId uuid.UUID) Transaction {
	source := uuid.New()
	return Transaction{
		Id: uuid.New(), WalletId: walletId, SourceId: &source,
		Operation: OperationDebit, State: "COMPLETE",
		Value: 5000, Balance: 15000, Message: "перевод",
		CreatedAt: time.Now().UTC(),
	}
}

// TestOperationError фиксирует перевод ошибок домена в коды gRPC: без него
// клиент на любую причину получает Unknown и не может отличить нехватку
// средств от сбоя базы.
func TestOperationError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"неположительная сумма", ErrInvalidValue, codes.InvalidArgument},
		{"перевод самому себе", ErrSameWallet, codes.InvalidArgument},
		{"кошелька нет", ErrWalletNotFound, codes.NotFound},
		{"резерва нет", ErrReservationNotFound, codes.NotFound},
		{"резерв уже завершён", ErrReservationNotPending, codes.FailedPrecondition},
		{"кошелёк не активен", ErrWalletNotActive, codes.FailedPrecondition},
		{"не хватает средств", ErrInsufficientBalance, codes.FailedPrecondition},
		{"сбой базы", errors.New("connection refused"), codes.Internal},
		{"обёрнутая ошибка домена", errors.Join(errors.New("списание"), ErrInsufficientBalance), codes.FailedPrecondition},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := status.Code(operationError(context.Background(), test.err)); got != test.want {
				t.Errorf("код %s, ожидался %s", got, test.want)
			}
		})
	}

	// Сбой базы наружу не пересказывается: сообщение раскрывает внутренности.
	err := operationError(context.Background(), errors.New("pq: relation transaction does not exist"))
	if message := status.Convert(err).Message(); message != "operation failed" {
		t.Errorf("сообщение %q, ожидалось общее", message)
	}
}

func TestUnauthenticated(t *testing.T) {
	s := service{db: &fakeDatabase{}}
	ctx := context.Background()

	calls := map[string]func() error{
		"Information": func() error {
			_, err := s.Information(ctx, &wallet.InformationRequest{})
			return err
		},
		"Debit": func() error {
			_, err := s.Debit(ctx, &wallet.OperationRequest{IdempotencyKey: "k"})
			return err
		},
		"Credit": func() error {
			_, err := s.Credit(ctx, &wallet.OperationRequest{IdempotencyKey: "k"})
			return err
		},
		"Transfer": func() error {
			_, err := s.Transfer(ctx, &wallet.TransferRequest{IdempotencyKey: "k"})
			return err
		},
		"History": func() error {
			_, err := s.History(ctx, &wallet.HistoryRequest{})
			return err
		},
		"ChangeState": func() error {
			_, err := s.ChangeState(ctx, &wallet.ChangeStateRequest{})
			return err
		},
		"Reserve": func() error {
			_, err := s.Reserve(ctx, &wallet.ReserveRequest{IdempotencyKey: "k"})
			return err
		},
		"Confirm": func() error {
			_, err := s.Confirm(ctx, &wallet.SettleRequest{})
			return err
		},
		"Reject": func() error {
			_, err := s.Reject(ctx, &wallet.SettleRequest{})
			return err
		},
	}

	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if got := status.Code(call()); got != codes.Unauthenticated {
				t.Errorf("код %s, ожидался %s", got, codes.Unauthenticated)
			}
		})
	}
}

func TestInformation(t *testing.T) {
	owner := uuid.New()
	walletId := uuid.New()
	db := &fakeDatabase{wallets: []*wallet.InformationReply{
		{Id: walletId.String(), Type: wallet.WalletType_USER, State: wallet.WalletState_ACTIVE, Balance: 100},
	}}
	s := service{db: db}

	t.Run("свои кошельки", func(t *testing.T) {
		replies, err := s.Information(callerContext(t, owner), &wallet.InformationRequest{})
		if err != nil {
			t.Fatalf("чтение: %v", err)
		}
		if len(replies.GetReplies()) != 1 {
			t.Fatalf("кошельков %d, ожидался один", len(replies.GetReplies()))
		}
		if db.owner != owner {
			t.Errorf("запрошены кошельки %s, ожидались %s", db.owner, owner)
		}
	})

	t.Run("чужой кошелёк оператору", func(t *testing.T) {
		other := uuid.New()
		id := other.String()
		if _, err := s.Information(callerContext(t, uuid.New(), services.RoleOperator),
			&wallet.InformationRequest{UserId: &id}); err != nil {
			t.Fatalf("чтение оператором: %v", err)
		}
		if db.owner != other {
			t.Errorf("запрошены кошельки %s, ожидались %s", db.owner, other)
		}
	})

	t.Run("чужой кошелёк постороннему запрещён", func(t *testing.T) {
		id := uuid.New().String()
		_, err := s.Information(callerContext(t, uuid.New()), &wallet.InformationRequest{UserId: &id})
		if got := status.Code(err); got != codes.PermissionDenied {
			t.Errorf("код %s, ожидался %s", got, codes.PermissionDenied)
		}
	})

	t.Run("идентификатор не разбирается", func(t *testing.T) {
		id := "не-uuid"
		_, err := s.Information(callerContext(t, owner), &wallet.InformationRequest{UserId: &id})
		if got := status.Code(err); got != codes.InvalidArgument {
			t.Errorf("код %s, ожидался %s", got, codes.InvalidArgument)
		}
	})

	t.Run("сбой базы", func(t *testing.T) {
		failing := service{db: &fakeDatabase{err: errors.New("connection refused")}}
		_, err := failing.Information(callerContext(t, owner), &wallet.InformationRequest{})
		if got := status.Code(err); got != codes.Internal {
			t.Errorf("код %s, ожидался %s", got, codes.Internal)
		}
	})
}

func TestDebitAndCredit(t *testing.T) {
	owner := uuid.New()
	walletId := uuid.New()

	for name, call := range map[string]func(service, context.Context, *wallet.OperationRequest) (*wallet.TransactionReply, error){
		"Debit": func(s service, ctx context.Context, r *wallet.OperationRequest) (*wallet.TransactionReply, error) {
			return s.Debit(ctx, r)
		},
		"Credit": func(s service, ctx context.Context, r *wallet.OperationRequest) (*wallet.TransactionReply, error) {
			return s.Credit(ctx, r)
		},
	} {
		t.Run(name, func(t *testing.T) {
			id := walletId.String()
			db := &fakeDatabase{transaction: transaction(walletId)}
			s := service{db: db}

			reply, err := call(s, callerContext(t, owner), &wallet.OperationRequest{
				IdempotencyKey: "op-1", WalletId: &id, Value: 5000, Message: "перевод",
			})
			if err != nil {
				t.Fatalf("операция: %v", err)
			}
			if reply.GetWalletId() != walletId.String() {
				t.Errorf("кошелёк в ответе %q, ожидался %q", reply.GetWalletId(), walletId)
			}
			if reply.GetSourceWalletId() == "" {
				t.Error("исходный кошелёк потерян в ответе")
			}
			if db.owner != owner {
				t.Errorf("операция от имени %s, ожидался %s", db.owner, owner)
			}

			params, ok := db.params.(OperationParams)
			if !ok {
				t.Fatalf("в репозиторий ушло %T", db.params)
			}
			if params.IdempotencyKey != "op-1" || params.WalletId != walletId || params.Value != 5000 {
				t.Errorf("параметры %+v", params)
			}
		})

		t.Run(name+" без ключа идемпотентности", func(t *testing.T) {
			// Без ключа повтор при обрыве связи проведёт операцию второй раз,
			// а клиент не может отличить «запрос не дошёл» от «ответ не дошёл».
			s := service{db: &fakeDatabase{}}
			_, err := call(s, callerContext(t, owner), &wallet.OperationRequest{Value: 100})
			if got := status.Code(err); got != codes.InvalidArgument {
				t.Errorf("код %s, ожидался %s", got, codes.InvalidArgument)
			}
		})

		t.Run(name+" с неразбираемым кошельком", func(t *testing.T) {
			bad := "не-uuid"
			s := service{db: &fakeDatabase{}}
			_, err := call(s, callerContext(t, owner), &wallet.OperationRequest{
				IdempotencyKey: "op-1", WalletId: &bad,
			})
			if got := status.Code(err); got != codes.InvalidArgument {
				t.Errorf("код %s, ожидался %s", got, codes.InvalidArgument)
			}
		})

		t.Run(name+" с отказом репозитория", func(t *testing.T) {
			s := service{db: &fakeDatabase{err: ErrInsufficientBalance}}
			_, err := call(s, callerContext(t, owner), &wallet.OperationRequest{
				IdempotencyKey: "op-1", Value: 100,
			})
			if got := status.Code(err); got != codes.FailedPrecondition {
				t.Errorf("код %s, ожидался %s", got, codes.FailedPrecondition)
			}
		})
	}
}

// TestParseWalletIdEmpty: пустой идентификатор означает кошелёк по умолчанию,
// а не ошибку — иначе клиенту пришлось бы сначала его узнавать.
func TestParseWalletIdEmpty(t *testing.T) {
	empty := ""
	for _, value := range []*string{nil, &empty} {
		id, err := parseWalletId(value)
		if err != nil {
			t.Fatalf("пустой идентификатор отклонён: %v", err)
		}
		if id != uuid.Nil {
			t.Errorf("получен %s, ожидался нулевой", id)
		}
	}
}

func TestTransfer(t *testing.T) {
	owner := uuid.New()
	source := uuid.New()
	target := uuid.New()

	t.Run("перевод уходит в репозиторий целиком", func(t *testing.T) {
		db := &fakeDatabase{transaction: transaction(source)}
		s := service{db: db}

		if _, err := s.Transfer(callerContext(t, owner), &wallet.TransferRequest{
			IdempotencyKey: "tr-1",
			SourceWalletId: source.String(),
			TargetWalletId: target.String(),
			Value:          5000,
			Message:        "подарок",
		}); err != nil {
			t.Fatalf("перевод: %v", err)
		}

		params, ok := db.params.(TransferParams)
		if !ok {
			t.Fatalf("в репозиторий ушло %T", db.params)
		}
		if params.SourceId != source || params.TargetId != target || params.Value != 5000 {
			t.Errorf("параметры %+v", params)
		}
	})

	tests := []struct {
		name    string
		request *wallet.TransferRequest
		want    codes.Code
	}{
		{
			name:    "без ключа идемпотентности",
			request: &wallet.TransferRequest{SourceWalletId: source.String(), TargetWalletId: target.String()},
			want:    codes.InvalidArgument,
		},
		{
			name:    "исходный кошелёк не разбирается",
			request: &wallet.TransferRequest{IdempotencyKey: "tr-1", SourceWalletId: "не-uuid", TargetWalletId: target.String()},
			want:    codes.InvalidArgument,
		},
		{
			name:    "целевой кошелёк не разбирается",
			request: &wallet.TransferRequest{IdempotencyKey: "tr-1", SourceWalletId: source.String(), TargetWalletId: "не-uuid"},
			want:    codes.InvalidArgument,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := service{db: &fakeDatabase{}}
			_, err := s.Transfer(callerContext(t, owner), test.request)
			if got := status.Code(err); got != test.want {
				t.Errorf("код %s, ожидался %s", got, test.want)
			}
		})
	}

	t.Run("отказ репозитория", func(t *testing.T) {
		s := service{db: &fakeDatabase{err: ErrSameWallet}}
		_, err := s.Transfer(callerContext(t, owner), &wallet.TransferRequest{
			IdempotencyKey: "tr-1", SourceWalletId: source.String(), TargetWalletId: source.String(),
		})
		if got := status.Code(err); got != codes.InvalidArgument {
			t.Errorf("код %s, ожидался %s", got, codes.InvalidArgument)
		}
	})
}

// TestHistoryCursor фиксирует правило постраничной выдачи: курсор отдаётся,
// только если страница заполнена целиком — иначе клиент делает ещё один
// заведомо пустой запрос.
func TestHistoryCursor(t *testing.T) {
	owner := uuid.New()
	walletId := uuid.New()

	tests := []struct {
		name       string
		count      int
		limit      int32
		wantCursor bool
	}{
		{"страница заполнена целиком", 2, 2, true},
		{"страница неполная", 1, 2, false},
		{"пустая страница", 0, 2, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			history := make([]Transaction, 0, test.count)
			for range test.count {
				history = append(history, transaction(walletId))
			}
			s := service{db: &fakeDatabase{history: history}}

			reply, err := s.History(callerContext(t, owner), &wallet.HistoryRequest{Limit: test.limit})
			if err != nil {
				t.Fatalf("история: %v", err)
			}
			if len(reply.GetTransactions()) != test.count {
				t.Errorf("транзакций %d, ожидалось %d", len(reply.GetTransactions()), test.count)
			}
			if hasCursor := reply.GetNextBefore() != nil; hasCursor != test.wantCursor {
				t.Errorf("курсор %v, ожидался %v", hasCursor, test.wantCursor)
			}
		})
	}
}

func TestHistoryParams(t *testing.T) {
	owner := uuid.New()
	id := uuid.New().String()
	before := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	db := &fakeDatabase{}
	s := service{db: db}

	if _, err := s.History(callerContext(t, owner), &wallet.HistoryRequest{
		WalletId: &id, Limit: 10, Before: timestamppb.New(before),
	}); err != nil {
		t.Fatalf("история: %v", err)
	}
	if db.historyLimit != 10 {
		t.Errorf("предел %d, ожидалось 10", db.historyLimit)
	}
	if db.historyBefore == nil || !db.historyBefore.Equal(before) {
		t.Errorf("курсор %v, ожидался %s", db.historyBefore, before)
	}

	t.Run("неразбираемый кошелёк", func(t *testing.T) {
		bad := "не-uuid"
		_, err := s.History(callerContext(t, owner), &wallet.HistoryRequest{WalletId: &bad})
		if got := status.Code(err); got != codes.InvalidArgument {
			t.Errorf("код %s, ожидался %s", got, codes.InvalidArgument)
		}
	})

	t.Run("сбой базы", func(t *testing.T) {
		failing := service{db: &fakeDatabase{err: errors.New("connection refused")}}
		_, err := failing.History(callerContext(t, owner), &wallet.HistoryRequest{})
		if got := status.Code(err); got != codes.Internal {
			t.Errorf("код %s, ожидался %s", got, codes.Internal)
		}
	})
}

func TestChangeState(t *testing.T) {
	owner := uuid.New()
	walletId := uuid.New()

	t.Run("состояние меняется и кошелёк возвращается", func(t *testing.T) {
		db := &fakeDatabase{wallets: []*wallet.InformationReply{
			{Id: walletId.String(), State: wallet.WalletState_BLOCKED},
		}}
		s := service{db: db}

		reply, err := s.ChangeState(callerContext(t, owner), &wallet.ChangeStateRequest{
			WalletId: walletId.String(), State: wallet.WalletState_BLOCKED,
		})
		if err != nil {
			t.Fatalf("смена состояния: %v", err)
		}
		if reply.GetState() != wallet.WalletState_BLOCKED {
			t.Errorf("состояние %s, ожидалось BLOCKED", reply.GetState())
		}
		if db.stateChanged != wallet.WalletState_BLOCKED.String() {
			t.Errorf("в репозиторий ушло %q", db.stateChanged)
		}
	})

	tests := []struct {
		name    string
		request *wallet.ChangeStateRequest
		db      *fakeDatabase
		want    codes.Code
	}{
		{
			name:    "кошелёк не разбирается",
			request: &wallet.ChangeStateRequest{WalletId: "не-uuid", State: wallet.WalletState_BLOCKED},
			db:      &fakeDatabase{},
			want:    codes.InvalidArgument,
		},
		{
			name:    "состояние не задано",
			request: &wallet.ChangeStateRequest{WalletId: walletId.String()},
			db:      &fakeDatabase{},
			want:    codes.InvalidArgument,
		},
		{
			name:    "отказ репозитория",
			request: &wallet.ChangeStateRequest{WalletId: walletId.String(), State: wallet.WalletState_BLOCKED},
			db:      &fakeDatabase{err: ErrWalletNotFound},
			want:    codes.NotFound,
		},
		{
			// Состояние сменилось, а кошелёк среди своих не нашёлся:
			// отвечать успехом здесь нельзя.
			name:    "кошелёк не нашёлся после смены",
			request: &wallet.ChangeStateRequest{WalletId: walletId.String(), State: wallet.WalletState_BLOCKED},
			db:      &fakeDatabase{},
			want:    codes.NotFound,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := service{db: test.db}
			_, err := s.ChangeState(callerContext(t, owner), test.request)
			if got := status.Code(err); got != test.want {
				t.Errorf("код %s, ожидался %s", got, test.want)
			}
		})
	}
}

func TestReserveAndSettle(t *testing.T) {
	owner := uuid.New()
	walletId := uuid.New()
	transactionId := uuid.New()

	t.Run("резерв уходит в репозиторий со сроком жизни", func(t *testing.T) {
		id := walletId.String()
		db := &fakeDatabase{transaction: transaction(walletId)}
		s := service{db: db}

		if _, err := s.Reserve(callerContext(t, owner), &wallet.ReserveRequest{
			IdempotencyKey: "res-1", WalletId: &id, Value: 5000, TtlSeconds: 120,
		}); err != nil {
			t.Fatalf("резерв: %v", err)
		}

		params, ok := db.params.(ReserveParams)
		if !ok {
			t.Fatalf("в репозиторий ушло %T", db.params)
		}
		if params.TTL != 2*time.Minute {
			t.Errorf("срок жизни %s, ожидались две минуты", params.TTL)
		}
	})

	t.Run("резерв без ключа идемпотентности", func(t *testing.T) {
		s := service{db: &fakeDatabase{}}
		_, err := s.Reserve(callerContext(t, owner), &wallet.ReserveRequest{Value: 100})
		if got := status.Code(err); got != codes.InvalidArgument {
			t.Errorf("код %s, ожидался %s", got, codes.InvalidArgument)
		}
	})

	t.Run("резерв с неразбираемым кошельком", func(t *testing.T) {
		bad := "не-uuid"
		s := service{db: &fakeDatabase{}}
		_, err := s.Reserve(callerContext(t, owner), &wallet.ReserveRequest{
			IdempotencyKey: "res-1", WalletId: &bad,
		})
		if got := status.Code(err); got != codes.InvalidArgument {
			t.Errorf("код %s, ожидался %s", got, codes.InvalidArgument)
		}
	})

	t.Run("резерв с отказом репозитория", func(t *testing.T) {
		s := service{db: &fakeDatabase{err: ErrInsufficientBalance}}
		_, err := s.Reserve(callerContext(t, owner), &wallet.ReserveRequest{
			IdempotencyKey: "res-1", Value: 100,
		})
		if got := status.Code(err); got != codes.FailedPrecondition {
			t.Errorf("код %s, ожидался %s", got, codes.FailedPrecondition)
		}
	})

	for name, call := range map[string]func(service, context.Context, *wallet.SettleRequest) (*wallet.TransactionReply, error){
		"Confirm": func(s service, ctx context.Context, r *wallet.SettleRequest) (*wallet.TransactionReply, error) {
			return s.Confirm(ctx, r)
		},
		"Reject": func(s service, ctx context.Context, r *wallet.SettleRequest) (*wallet.TransactionReply, error) {
			return s.Reject(ctx, r)
		},
	} {
		t.Run(name, func(t *testing.T) {
			db := &fakeDatabase{transaction: transaction(walletId)}
			s := service{db: db}

			if _, err := call(s, callerContext(t, owner),
				&wallet.SettleRequest{TransactionId: transactionId.String()}); err != nil {
				t.Fatalf("завершение резерва: %v", err)
			}
			if db.params != transactionId {
				t.Errorf("в репозиторий ушла транзакция %v, ожидалась %s", db.params, transactionId)
			}
		})

		t.Run(name+" с неразбираемой транзакцией", func(t *testing.T) {
			s := service{db: &fakeDatabase{}}
			_, err := call(s, callerContext(t, owner), &wallet.SettleRequest{TransactionId: "не-uuid"})
			if got := status.Code(err); got != codes.InvalidArgument {
				t.Errorf("код %s, ожидался %s", got, codes.InvalidArgument)
			}
		})

		t.Run(name+" с отказом репозитория", func(t *testing.T) {
			s := service{db: &fakeDatabase{err: ErrReservationNotPending}}
			_, err := call(s, callerContext(t, owner),
				&wallet.SettleRequest{TransactionId: transactionId.String()})
			if got := status.Code(err); got != codes.FailedPrecondition {
				t.Errorf("код %s, ожидался %s", got, codes.FailedPrecondition)
			}
		})
	}
}

// TestTransactionReplyWithoutSource: у зачисления исходного кошелька нет,
// и в ответе поле обязано остаться пустым, а не содержать нулевой uuid.
func TestTransactionReplyWithoutSource(t *testing.T) {
	t.Run("без исходного кошелька", func(t *testing.T) {
		reply := transactionReply(Transaction{
			Id: uuid.New(), WalletId: uuid.New(),
			Operation: OperationDebit, State: "COMPLETE", CreatedAt: time.Now(),
		})
		if reply.SourceWalletId != nil {
			t.Errorf("исходный кошелёк %q, ожидалось пусто", *reply.SourceWalletId)
		}
	})
}
