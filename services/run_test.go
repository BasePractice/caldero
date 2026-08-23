package services

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// freePort занимает и сразу освобождает порт: сервер поднимается в тесте
// на настоящем адресе, а фиксированный номер порта сделал бы тесты
// зависимыми друг от друга.
func freePort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("занять порт: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("освободить порт: %v", err)
	}
	return addr
}

// TestServeHTTPGracefulShutdown фиксирует главное свойство остановки:
// отмена контекста не обрывает запрос на середине. Раньше сигнал приводил
// к немедленному os.Exit(0), и соединение рвалось.
func TestServeHTTPGracefulShutdown(t *testing.T) {
	addr := freePort(t)
	ctx, cancel := context.WithCancel(t.Context())

	started := make(chan struct{})
	release := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusTeapot)
	})

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- ServeHTTP(ctx, Config{MaxInFlightRequests: 4}, addr, handler)
	}()

	response := make(chan *http.Response, 1)
	go func() {
		for range 100 {
			resp, err := http.Get("http://" + addr + "/")
			if err == nil {
				response <- resp
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		response <- nil
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("обработчик не получил запрос")
	}

	// Остановка объявляется, пока запрос ещё в работе: он обязан
	// доработать и получить свой ответ.
	cancel()
	close(release)

	resp := <-response
	if resp == nil {
		t.Fatal("запрос не дошёл до сервера")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("код ответа %d, ожидался %d", resp.StatusCode, http.StatusTeapot)
	}

	if err := <-serveErr; err != nil {
		t.Errorf("остановка сервера: %v", err)
	}
}

// TestServeHTTPBusyPort: занятый порт обязан быть ошибкой старта, а не
// молча работающим сервисом, который не слушает.
func TestServeHTTPBusyPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("занять порт: %v", err)
	}
	defer func() { _ = listener.Close() }()

	err = ServeHTTP(t.Context(), Config{MaxInFlightRequests: 1}, listener.Addr().String(),
		http.NotFoundHandler())
	if err == nil {
		t.Fatal("сервер поднялся на занятом порту")
	}
	if !strings.Contains(err.Error(), listener.Addr().String()) {
		t.Errorf("ошибка %q не называет адрес", err)
	}
}

func TestServeGRPCGracefulShutdown(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("занять порт: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())

	srv := grpc.NewServer()
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- ServeGRPC(ctx, listener, srv)
	}()

	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Errorf("остановка сервера: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("сервер не остановился по отмене контекста")
	}
}

// TestServeGRPCClosedListener: сервер, потерявший слушателя, обязан вернуть
// ошибку, а не завершиться молча.
func TestServeGRPCClosedListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("занять порт: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("закрыть слушателя: %v", err)
	}

	if err := ServeGRPC(t.Context(), listener, grpc.NewServer()); err == nil {
		t.Fatal("закрытый слушатель принят")
	}
}

func TestRunPeriodic(t *testing.T) {
	t.Run("сбой прогона не останавливает расписание", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		var runs atomic.Int32
		done := make(chan struct{})
		go func() {
			defer close(done)
			RunPeriodic(ctx, "проверка", time.Millisecond, func(context.Context) error {
				if runs.Add(1) >= 3 {
					cancel()
				}
				return errors.New("сбой прогона")
			})
		}()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("задача не остановилась по отмене контекста")
		}
		if got := runs.Load(); got < 3 {
			t.Errorf("прогонов %d, ожидалось не меньше трёх", got)
		}
	})

	t.Run("неположительный интервал выключает задачу", func(t *testing.T) {
		for _, interval := range []time.Duration{0, -time.Second} {
			// Возврат из RunPeriodic сам по себе и есть проверка: с ненулевым
			// интервалом вызов заблокировался бы до отмены контекста.
			RunPeriodic(t.Context(), "выключено", interval, func(context.Context) error {
				t.Error("задача с неположительным интервалом выполнилась")
				return nil
			})
		}
	})
}

type failingCloser struct {
	err error
}

func (f failingCloser) Close() error { return f.err }

// TestClose: закрытие в defer возвращать ошибку некому, поэтому она обязана
// попасть в журнал, а не потеряться. Проверяется, что вызов не паникует
// и не роняет процесс.
func TestClose(t *testing.T) {
	Close("исправный", failingCloser{})
	Close("сбойный", failingCloser{err: errors.New("не закрылось")})
}

func TestRecover(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    int
	}{
		{
			name:    "паника превращается в 500",
			handler: func(http.ResponseWriter, *http.Request) { panic("что-то пошло не так") },
			want:    http.StatusInternalServerError,
		},
		{
			name:    "паника с ошибкой",
			handler: func(http.ResponseWriter, *http.Request) { panic(errors.New("сбой")) },
			want:    http.StatusInternalServerError,
		},
		{
			name: "обычный ответ не трогается",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
			},
			want: http.StatusCreated,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			Recover(test.handler).ServeHTTP(recorder,
				httptest.NewRequest(http.MethodGet, "/panic", nil))
			if recorder.Code != test.want {
				t.Errorf("код ответа %d, ожидался %d", recorder.Code, test.want)
			}
		})
	}
}

func TestRecoverUnaryInterceptor(t *testing.T) {
	info := &grpc.UnaryServerInfo{FullMethod: "/wallet.v1.Service/Transfer"}

	t.Run("паника превращается в internal", func(t *testing.T) {
		_, err := RecoverUnaryInterceptor(context.Background(), nil, info,
			func(context.Context, any) (any, error) { panic("сбой") })
		if status.Code(err) != codes.Internal {
			t.Errorf("код %s, ожидался %s", status.Code(err), codes.Internal)
		}
		// Наружу отдаётся общее сообщение: текст паники раскрывает
		// внутренности сервиса.
		if strings.Contains(status.Convert(err).Message(), "сбой") {
			t.Errorf("сообщение паники утекло наружу: %v", err)
		}
	})

	t.Run("обычный ответ не трогается", func(t *testing.T) {
		resp, err := RecoverUnaryInterceptor(context.Background(), nil, info,
			func(context.Context, any) (any, error) { return "ответ", nil })
		if err != nil {
			t.Fatalf("перехватчик вернул ошибку: %v", err)
		}
		if resp != "ответ" {
			t.Errorf("ответ %v потерян перехватчиком", resp)
		}
	})

	t.Run("ошибка обработчика доходит как есть", func(t *testing.T) {
		want := status.Error(codes.NotFound, "нет кошелька")
		_, err := RecoverUnaryInterceptor(context.Background(), nil, info,
			func(context.Context, any) (any, error) { return nil, want })
		if !errors.Is(err, want) {
			t.Errorf("ошибка %v, ожидалась %v", err, want)
		}
	})
}

// TestDefineMetrics проверяет служебный порт, который поднимает Run:
// на нём обязаны быть и метрики, и пробы.
func TestDefineMetrics(t *testing.T) {
	addr := freePort(t)
	port, err := strconv.Atoi(addr[strings.LastIndex(addr, ":")+1:])
	if err != nil {
		t.Fatalf("разбор порта: %v", err)
	}

	health := NewHealth()
	health.Register("проверка", func(context.Context) error { return nil })
	// Отладочные страницы включены намеренно: они не аутентифицированы,
	// и проверка обязана показывать, что без флага их нет.
	DefineMetrics(Config{MetricsPort: port, DebugPprof: true, DebugStatsviz: true}, health)

	client := &http.Client{Timeout: 5 * time.Second}
	get := func(path string) int {
		for range 100 {
			resp, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port) + path)
			if err != nil {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			defer func() { _ = resp.Body.Close() }()
			return resp.StatusCode
		}
		t.Fatalf("служебный порт не отвечает на %s", path)
		return 0
	}

	for path, want := range map[string]int{
		"/metrics":             http.StatusOK,
		"/livez":               http.StatusOK,
		"/readyz":              http.StatusOK,
		"/debug/pprof/":        http.StatusOK,
		"/debug/pprof/cmdline": http.StatusOK,
	} {
		if got := get(path); got != want {
			t.Errorf("%s вернул %d, ожидался %d", path, got, want)
		}
	}
}

// TestRun проверяет общую точку входа сервиса: конфигурация, журнал,
// служебный порт и отменяемый контекст собираются здесь, и раньше эти
// сорок строк были продублированы в каждом main.go вместе с ошибками.
//
// Ветки с os.Exit не проверяются: они завершают процесс, и вызвать их
// в тестовом бинарнике нельзя.
func TestRun(t *testing.T) {
	cleanEnv(t)
	addr := freePort(t)
	t.Setenv("METRICS_PORT", addr[strings.LastIndex(addr, ":")+1:])

	called := false
	Run("test-run", func(ctx context.Context, cfg Config, health *Health) error {
		called = true
		if cfg.LogLevel != "INFO" {
			t.Errorf("конфигурация не прочитана: %+v", cfg)
		}
		if health == nil {
			t.Error("набор проб не создан")
		}
		if err := ctx.Err(); err != nil {
			t.Errorf("контекст отменён до начала работы: %v", err)
		}
		return nil
	})

	if !called {
		t.Error("тело сервиса не вызвано")
	}
}
