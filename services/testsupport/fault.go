//go:build integration

package testsupport

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"testing"

	"wish/services"

	"github.com/lib/pq"
)

// ErrFault — ошибка, которую драйвер возвращает вместо результата запроса.
// Отдельный тип нужен, чтобы тест отличал внедрённый сбой от настоящей
// ошибки базы: совпадение по тексту здесь было бы гаданием.
//
// Это намеренно не driver.ErrBadConn: по нему database/sql повторил бы
// запрос на другом соединении, и сбой бы не дошёл до вызывающего кода.
var ErrFault = errors.New("внедрённый сбой базы")

// Fault считает запросы и роняет запрос с назначенным номером.
//
// Смысл не в самом падении, а в том, где оно происходит: закрытая база
// валит первый же запрос, до входа в транзакцию, а охранные ветви внутри
// транзакций проходятся только сбоем посреди неё.
//
// Считается всё, что идёт к серверу и может не дойти: начало транзакции,
// запросы, подготовка выражений и фиксация. Не считается только откат —
// он обязан работать всегда, иначе после сбоя не убрать за собой.
type Fault struct {
	mu        sync.Mutex
	countdown int
	fired     bool
}

// FailAt взводит сбой на n-м по счёту запросе, начиная с текущего момента.
// Предыдущий отсчёт при этом сбрасывается.
func (f *Fault) FailAt(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.countdown = n
	f.fired = false
}

// Disarm снимает сбой: дальше запросы идут в базу как обычно. Нужен после
// операции, которая закончилась раньше, чем дошла до назначенного запроса —
// иначе взведённый сбой достался бы следующей проверке состояния.
func (f *Fault) Disarm() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.countdown = 0
}

// Fired сообщает, сработал ли сбой после последнего FailAt.
func (f *Fault) Fired() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fired
}

// check вызывается перед каждым запросом. Сработавший сбой снимается сразу:
// откат транзакции и последующая проверка состояния обязаны дойти до базы.
func (f *Fault) check() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.countdown == 0 {
		return nil
	}
	if f.countdown--; f.countdown > 0 {
		return nil
	}
	f.fired = true
	return ErrFault
}

// OpenFaulty открывает второе подключение к той же базе через обёртку над
// драйвером. Схему при этом никто не создаёт и миграции не применяет: это
// делает обычный конструктор репозитория, а сюда приходит уже готовая база.
//
// Пул ограничен одним соединением: с несколькими номер запроса зависел бы
// от того, какое соединение досталось операции.
func OpenFaulty(t *testing.T, cfg services.Config) (*sql.DB, *Fault) {
	t.Helper()

	base, err := pq.NewConnector(cfg.PostgresURL)
	if err != nil {
		t.Fatalf("не удалось разобрать строку подключения: %v", err)
	}
	fault := &Fault{}
	db := sql.OpenDB(faultyConnector{base: base, fault: fault})
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Logf("не удалось закрыть подключение со сбоями: %v", err)
		}
	})

	if err = db.PingContext(context.Background()); err != nil {
		t.Fatalf("не удалось подключиться: %v", err)
	}
	return db, fault
}

type faultyConnector struct {
	base  driver.Connector
	fault *Fault
}

func (c faultyConnector) Connect(ctx context.Context) (driver.Conn, error) {
	base, err := c.base.Connect(ctx)
	if err != nil {
		return nil, fmt.Errorf("подключение к базе: %w", err)
	}
	inner, ok := base.(faultyBase)
	if !ok {
		// Обёртка рассчитана на pq: без контекстных интерфейсов
		// database/sql молча ушёл бы на подготовленные выражения,
		// и счёт запросов перестал бы совпадать с шагами операции.
		_ = base.Close()
		return nil, fmt.Errorf("драйвер %T не поддерживает нужные интерфейсы", base)
	}
	return faultyConn{base: inner, fault: c.fault}, nil
}

func (c faultyConnector) Driver() driver.Driver { return c.base.Driver() }

// faultyBase — набор интерфейсов соединения, на которые рассчитана обёртка.
// Все они реализованы в pq; проверка при подключении делает это требование
// явным, а не превращает его в тихую смену поведения.
type faultyBase interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.ExecerContext
	driver.QueryerContext
	driver.Pinger
	driver.SessionResetter
	driver.Validator
}

type faultyConn struct {
	base  faultyBase
	fault *Fault
}

func (c faultyConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c faultyConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if err := c.fault.check(); err != nil {
		return nil, err
	}
	base, err := c.base.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	return faultyStmt{base: base, fault: c.fault}, nil
}

func (c faultyConn) ExecContext(
	ctx context.Context,
	query string,
	args []driver.NamedValue,
) (driver.Result, error) {
	if err := c.fault.check(); err != nil {
		return nil, err
	}
	return c.base.ExecContext(ctx, query, args)
}

func (c faultyConn) QueryContext(
	ctx context.Context,
	query string,
	args []driver.NamedValue,
) (driver.Rows, error) {
	if err := c.fault.check(); err != nil {
		return nil, err
	}
	return c.base.QueryContext(ctx, query, args)
}

func (c faultyConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c faultyConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if err := c.fault.check(); err != nil {
		return nil, err
	}
	base, err := c.base.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return faultyTx{base: base, fault: c.fault}, nil
}

func (c faultyConn) Close() error { return c.base.Close() }

func (c faultyConn) Ping(ctx context.Context) error { return c.base.Ping(ctx) }

func (c faultyConn) ResetSession(ctx context.Context) error { return c.base.ResetSession(ctx) }

func (c faultyConn) IsValid() bool { return c.base.IsValid() }

// faultyTx роняет фиксацию так же, как настоящая база: транзакция при этом
// откатывается. Вернуть ошибку, оставив транзакцию открытой, было бы враньём —
// соединение уходит обратно в пул, и следующий запрос попал бы в чужую
// незакрытую транзакцию.
type faultyTx struct {
	base  driver.Tx
	fault *Fault
}

func (t faultyTx) Commit() error {
	if err := t.fault.check(); err != nil {
		if rollbackErr := t.base.Rollback(); rollbackErr != nil {
			return fmt.Errorf("откат после внедрённого сбоя: %w", rollbackErr)
		}
		return err
	}
	return t.base.Commit()
}

func (t faultyTx) Rollback() error { return t.base.Rollback() }

// faultyStmt считает выполнение подготовленного выражения наравне с прямым
// запросом: repository пользуется и тем, и другим, а для проверяемого
// свойства разницы нет.
type faultyStmt struct {
	base  driver.Stmt
	fault *Fault
}

func (s faultyStmt) Close() error { return s.base.Close() }

func (s faultyStmt) NumInput() int { return s.base.NumInput() }

func (s faultyStmt) Exec(args []driver.Value) (driver.Result, error) {
	if err := s.fault.check(); err != nil {
		return nil, err
	}
	return s.base.Exec(args) //nolint:staticcheck // требуется интерфейсом driver.Stmt
}

func (s faultyStmt) Query(args []driver.Value) (driver.Rows, error) {
	if err := s.fault.check(); err != nil {
		return nil, err
	}
	return s.base.Query(args) //nolint:staticcheck // требуется интерфейсом driver.Stmt
}

func (s faultyStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	if err := s.fault.check(); err != nil {
		return nil, err
	}
	base, ok := s.base.(driver.StmtExecContext)
	if !ok {
		return nil, errors.New("подготовленное выражение не поддерживает ExecContext")
	}
	return base.ExecContext(ctx, args)
}

func (s faultyStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if err := s.fault.check(); err != nil {
		return nil, err
	}
	base, ok := s.base.(driver.StmtQueryContext)
	if !ok {
		return nil, errors.New("подготовленное выражение не поддерживает QueryContext")
	}
	return base.QueryContext(ctx, args)
}
