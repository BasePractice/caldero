package wallets

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	wallet "wish/middleware/wallet/v1"
	"wish/services"
	"wish/services/shared/credit"
)

// fakeService подменяет сгенерированный клиент кошелька. Реализуется руками
// и целиком: интерфейс сгенерирован, и наполовину реализовать его нельзя.
type fakeService struct {
	wallet.ServiceClient

	information *wallet.InformationReplyList
	informErr   error

	transferErr error
	// transferCtx и transferReq сохраняются, чтобы проверить, от чьего
	// имени ушёл вызов: перевод обязан идти от владельца, а чтение —
	// от служебного оператора.
	informCtx   context.Context
	transferCtx context.Context
	transferReq *wallet.TransferRequest
}

func (f *fakeService) Information(ctx context.Context, _ *wallet.InformationRequest, _ ...grpc.CallOption) (*wallet.InformationReplyList, error) {
	f.informCtx = ctx
	if f.informErr != nil {
		return nil, f.informErr
	}
	return f.information, nil
}

func (f *fakeService) Transfer(ctx context.Context, in *wallet.TransferRequest, _ ...grpc.CallOption) (*wallet.TransactionReply, error) {
	f.transferCtx = ctx
	f.transferReq = in
	if f.transferErr != nil {
		return nil, f.transferErr
	}
	return &wallet.TransactionReply{}, nil
}

func reply(id string, walletType wallet.WalletType, state wallet.WalletState, balance, available int64) *wallet.InformationReply {
	return &wallet.InformationReply{
		Id:        id,
		Type:      walletType,
		State:     state,
		Balance:   balance,
		Available: available,
	}
}

func TestWallet(t *testing.T) {
	walletId := uuid.New()
	owner := uuid.New()
	serviceId := uuid.New()

	fake := &fakeService{information: &wallet.InformationReplyList{
		Replies: []*wallet.InformationReply{
			// Чужой и неактивный кошельки должны быть пропущены:
			// у владельца может быть больше одного кошелька.
			reply(uuid.New().String(), wallet.WalletType_COMMON, wallet.WalletState_ACTIVE, 1, 1),
			reply(uuid.New().String(), wallet.WalletType_USER, wallet.WalletState_BLOCKED, 2, 2),
			reply(walletId.String(), wallet.WalletType_USER, wallet.WalletState_ACTIVE, 5000, 3000),
		},
	}}
	client := &Client{client: fake, serviceId: serviceId}

	info, err := client.Wallet(t.Context(), owner)
	if err != nil {
		t.Fatalf("чтение кошелька: %v", err)
	}
	if info.Id != walletId {
		t.Errorf("кошелёк %s, ожидался %s", info.Id, walletId)
	}
	if info.Balance != credit.Amount(5000) || info.Available != credit.Amount(3000) {
		t.Errorf("баланс %s (доступно %s), ожидалось 50.00 (30.00)", info.Balance, info.Available)
	}

	// Чужой кошелёк виден только оператору: без служебной роли сервис
	// кошелька откажет, и ошибка будет неочевидной.
	md, ok := metadata.FromOutgoingContext(fake.informCtx)
	if !ok {
		t.Fatal("вызов ушёл без метаданных авторизации")
	}
	if got := md.Get("x-authorized-id"); len(got) != 1 || got[0] != serviceId.String() {
		t.Errorf("вызов от имени %v, ожидался служебный %s", got, serviceId)
	}
	if got := md.Get("x-roles"); len(got) != 1 || got[0] != services.RoleOperator {
		t.Errorf("роли %v, ожидалась %s", got, services.RoleOperator)
	}
}

func TestWalletErrors(t *testing.T) {
	owner := uuid.New()

	tests := []struct {
		name string
		fake *fakeService
		want string
	}{
		{
			name: "сервис кошелька недоступен",
			fake: &fakeService{informErr: errors.New("connection refused")},
			want: "loading wallet",
		},
		{
			name: "активного кошелька нет",
			fake: &fakeService{information: &wallet.InformationReplyList{
				Replies: []*wallet.InformationReply{
					reply(uuid.New().String(), wallet.WalletType_USER, wallet.WalletState_BLOCKED, 0, 0),
				},
			}},
			want: "no active wallet",
		},
		{
			name: "пустой ответ",
			fake: &fakeService{information: &wallet.InformationReplyList{}},
			want: "no active wallet",
		},
		{
			name: "неразбираемый идентификатор кошелька",
			fake: &fakeService{information: &wallet.InformationReplyList{
				Replies: []*wallet.InformationReply{
					reply("не-uuid", wallet.WalletType_USER, wallet.WalletState_ACTIVE, 0, 0),
				},
			}},
			want: "parsing wallet id",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &Client{client: test.fake, serviceId: uuid.New()}

			_, err := client.Wallet(t.Context(), owner)
			if err == nil {
				t.Fatal("ошибка не возвращена")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("ошибка %q не содержит %q", err, test.want)
			}
		})
	}
}

func TestTransfer(t *testing.T) {
	owner := uuid.New()
	params := TransferParams{
		IdempotencyKey: "gift-42",
		Source:         uuid.New(),
		Target:         uuid.New(),
		Value:          credit.Amount(1500),
		Message:        "подарок",
	}

	fake := &fakeService{}
	client := &Client{client: fake, serviceId: uuid.New()}

	if err := client.Transfer(t.Context(), owner, params); err != nil {
		t.Fatalf("перевод: %v", err)
	}

	if fake.transferReq.GetIdempotencyKey() != params.IdempotencyKey {
		t.Errorf("ключ идемпотентности %q, ожидался %q",
			fake.transferReq.GetIdempotencyKey(), params.IdempotencyKey)
	}
	if fake.transferReq.GetSourceWalletId() != params.Source.String() {
		t.Errorf("исходный кошелёк %q, ожидался %q",
			fake.transferReq.GetSourceWalletId(), params.Source)
	}
	if fake.transferReq.GetTargetWalletId() != params.Target.String() {
		t.Errorf("целевой кошелёк %q, ожидался %q",
			fake.transferReq.GetTargetWalletId(), params.Target)
	}
	if fake.transferReq.GetValue() != int64(params.Value) {
		t.Errorf("сумма %d, ожидалась %d", fake.transferReq.GetValue(), params.Value)
	}
	if fake.transferReq.GetMessage() != params.Message {
		t.Errorf("сообщение %q, ожидалось %q", fake.transferReq.GetMessage(), params.Message)
	}

	// Перевод идёт от владельца, а не от служебного оператора: кошелёк
	// проверяет, что списывают у того, кто об этом просит.
	md, ok := metadata.FromOutgoingContext(fake.transferCtx)
	if !ok {
		t.Fatal("вызов ушёл без метаданных авторизации")
	}
	if got := md.Get("x-authorized-id"); len(got) != 1 || got[0] != owner.String() {
		t.Errorf("перевод от имени %v, ожидался владелец %s", got, owner)
	}
	if got := md.Get("x-roles"); len(got) != 0 {
		t.Errorf("перевод ушёл с ролями %v, ожидался без ролей", got)
	}
}

func TestTransferError(t *testing.T) {
	fake := &fakeService{transferErr: errors.New("insufficient funds")}
	client := &Client{client: fake, serviceId: uuid.New()}

	err := client.Transfer(t.Context(), uuid.New(), TransferParams{Value: credit.Amount(100)})
	if err == nil {
		t.Fatal("отказ кошелька не превратился в ошибку")
	}
	if !strings.Contains(err.Error(), "insufficient funds") {
		t.Errorf("ошибка %q потеряла причину отказа", err)
	}
}

// TestNewClient проверяет сборку клиента поверх соединения: grpc.NewClient
// соединение не устанавливает, поэтому живой сервис для этого не нужен.
func TestNewClient(t *testing.T) {
	conn, err := grpc.NewClient("passthrough:///wallet",
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("соединение: %v", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	serviceId := uuid.New()
	client := NewClient(conn, serviceId)
	if client.client == nil {
		t.Error("клиент кошелька не создан")
	}
	if client.serviceId != serviceId {
		t.Errorf("служебный идентификатор %s, ожидался %s", client.serviceId, serviceId)
	}
}

// TestWalletErrorSeparatesRefusalFromOutage: кошелёк переводит причины
// отказа в коды gRPC, и без обратного перевода эта работа пропадала —
// у вызывающего оставалась одна ошибка на всё. Нехватка средств
// выглядела как недоступность сервиса: человек видел «попробуйте позже»
// вместо «не хватает денег», а мониторинг — отказ там, где всё работало.
func TestWalletErrorSeparatesRefusalFromOutage(t *testing.T) {
	tests := []struct {
		name    string
		code    codes.Code
		message string
		want    error
	}{
		{
			name:    "не хватает средств",
			code:    codes.FailedPrecondition,
			message: "insufficient balance: available 0, requested 100",
			want:    ErrInsufficientFunds,
		},
		{
			// Тот же код, другая причина: повторять и пополнять
			// кошелёк одинаково бессмысленно.
			name:    "кошелёк заблокирован",
			code:    codes.FailedPrecondition,
			message: "wallet is not active: wallet is BLOCKED",
			want:    ErrRejected,
		},
		{
			name:    "чужой кошелёк",
			code:    codes.PermissionDenied,
			message: "wallet belongs to another user",
			want:    ErrRejected,
		},
		{
			name:    "кошелька нет",
			code:    codes.NotFound,
			message: "wallet not found",
			want:    ErrRejected,
		},
		{
			name:    "сбой сервиса",
			code:    codes.Internal,
			message: "operation failed",
			want:    ErrUnavailable,
		},
		{
			name:    "сервис не отвечает",
			code:    codes.Unavailable,
			message: "connection refused",
			want:    ErrUnavailable,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := walletError("проверка", status.Error(test.code, test.message))
			if !errors.Is(err, test.want) {
				t.Fatalf("получено %v, ожидалась %v", err, test.want)
			}
			// Причина должна дойти до вызывающего: без неё в журнале
			// остаётся только разряд ошибки.
			if !strings.Contains(err.Error(), test.message) {
				t.Errorf("в ошибке нет причины: %v", err)
			}
		})
	}
}

// TestTransferTranslatesRefusal: перевод отдаёт ту же разделённую ошибку,
// а не текст gRPC.
func TestTransferTranslatesRefusal(t *testing.T) {
	service := &fakeService{
		transferErr: status.Error(codes.FailedPrecondition,
			"insufficient balance: available 0, requested 100"),
	}
	client := &Client{client: service, serviceId: uuid.New()}

	err := client.Transfer(context.Background(), uuid.New(), TransferParams{
		IdempotencyKey: "key", Source: uuid.New(), Target: uuid.New(), Value: 100,
	})
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("получено %v, ожидалась %v", err, ErrInsufficientFunds)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Error("нехватка средств принята за недоступность кошелька")
	}
}
