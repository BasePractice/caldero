package services

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// requests возвращает значение счётчика запросов для набора меток. Метрики
// глобальные, поэтому тесты сравнивают приращение, а не абсолютное значение:
// порядок тестов на результат влиять не должен.
func requests(t *testing.T, labels prometheus.Labels) float64 {
	t.Helper()
	counter, err := requestsTotal.GetMetricWith(labels)
	if err != nil {
		t.Fatalf("метрика с метками %v: %v", labels, err)
	}
	return testutil.ToFloat64(counter)
}

// TestMeasureRouteLabel фиксирует главное свойство метки маршрута: она
// берётся из шаблона ServeMux, а не из пути. Иначе идентификатор в URL
// порождает новый временной ряд на каждый запрос.
func TestMeasureRouteLabel(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /account/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := Measure("test-route", mux)

	labels := prometheus.Labels{
		"service": "test-route", "transport": "http",
		"method": http.MethodGet, "route": "GET /account/{id}", "code": "200",
	}
	before := requests(t, labels)

	for _, id := range []string{"1", "2", "3"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/account/"+id, nil))
	}

	if got := requests(t, labels) - before; got != 3 {
		t.Errorf("приращение счётчика %v, ожидалось 3", got)
	}
}

func TestMeasureStatusCodes(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{
			// Обработчик, не вызвавший WriteHeader, отдаёт 200 — метрика
			// обязана записать то же самое, а не нулевой код.
			name:    "ответ без явного кода",
			handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) },
			want:    "200",
		},
		{
			name:    "пустой ответ без явного кода",
			handler: func(http.ResponseWriter, *http.Request) {},
			want:    "200",
		},
		{
			name: "явный отказ",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "нет", http.StatusNotFound)
			},
			want: "404",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /probe", test.handler)

			labels := prometheus.Labels{
				"service": "test-codes", "transport": "http",
				"method": http.MethodGet, "route": "GET /probe", "code": test.want,
			}
			before := requests(t, labels)

			recorder := httptest.NewRecorder()
			Measure("test-codes", mux).ServeHTTP(recorder,
				httptest.NewRequest(http.MethodGet, "/probe", nil))

			if got := requests(t, labels) - before; got != 1 {
				t.Errorf("приращение счётчика %v, ожидалось 1", got)
			}
		})
	}
}

// TestMeasureUnmatched: запрос, не подошедший ни под один маршрут, всё равно
// попадает в метрики — под меткой unmatched, а не под пустой.
func TestMeasureUnmatched(t *testing.T) {
	labels := prometheus.Labels{
		"service": "test-unmatched", "transport": "http",
		"method": http.MethodGet, "route": "unmatched", "code": "404",
	}
	before := requests(t, labels)

	recorder := httptest.NewRecorder()
	Measure("test-unmatched", http.NewServeMux()).ServeHTTP(recorder,
		httptest.NewRequest(http.MethodGet, "/nowhere", nil))

	if got := requests(t, labels) - before; got != 1 {
		t.Errorf("приращение счётчика %v, ожидалось 1", got)
	}
}

func TestMeasureUnaryInterceptor(t *testing.T) {
	interceptor := MeasureUnaryInterceptor("test-grpc")
	info := &grpc.UnaryServerInfo{FullMethod: "/wallet.v1.Service/Transfer"}

	tests := []struct {
		name string
		err  error
		code string
	}{
		{"успешный вызов", nil, codes.OK.String()},
		{"отказ со статусом", status.Error(codes.NotFound, "нет кошелька"), codes.NotFound.String()},
		{"ошибка без статуса", errors.New("сбой"), codes.Unknown.String()},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			labels := prometheus.Labels{
				"service": "test-grpc", "transport": "grpc",
				"method": "rpc", "route": info.FullMethod, "code": test.code,
			}
			before := requests(t, labels)

			resp, err := interceptor(context.Background(), "запрос", info,
				func(context.Context, any) (any, error) { return "ответ", test.err })

			if !errors.Is(err, test.err) {
				t.Errorf("ошибка %v, ожидалась %v", err, test.err)
			}
			if test.err == nil && resp != "ответ" {
				t.Errorf("ответ %v потерян перехватчиком", resp)
			}
			if got := requests(t, labels) - before; got != 1 {
				t.Errorf("приращение счётчика %v, ожидалось 1", got)
			}
		})
	}
}

type fakeStats struct {
	stats sql.DBStats
}

func (f fakeStats) Stats() sql.DBStats { return f.stats }

// TestRegisterDBStats проверяет, что метрики пула читают состояние на момент
// сбора, а не на момент регистрации: иначе исчерпание пула незаметно.
func TestRegisterDBStats(t *testing.T) {
	stats := &fakeStats{}
	RegisterDBStats("test-db-stats", stats)
	stats.stats = sql.DBStats{OpenConnections: 3, InUse: 2, WaitCount: 7}

	expected := `
# HELP wish_db_connections_in_use Соединений с БД, занятых запросами
# TYPE wish_db_connections_in_use gauge
wish_db_connections_in_use{service="test-db-stats"} 2
# HELP wish_db_connections_open Открытых соединений с БД
# TYPE wish_db_connections_open gauge
wish_db_connections_open{service="test-db-stats"} 3
# HELP wish_db_wait_total Сколько раз запрос ждал свободного соединения
# TYPE wish_db_wait_total counter
wish_db_wait_total{service="test-db-stats"} 7
`
	if err := testutil.GatherAndCompare(prometheus.DefaultGatherer, strings.NewReader(expected),
		"wish_db_connections_open", "wish_db_connections_in_use", "wish_db_wait_total"); err != nil {
		t.Error(err)
	}
}

func TestRegisterDefaultPartitionRows(t *testing.T) {
	rows := int64(0)
	RegisterDefaultPartitionRows("test-partition", func() int64 { return rows })
	// -1 означает, что подсчёт не удался: значение осмысленное и обязано
	// доезжать до метрики как есть, а не превращаться в ноль.
	rows = -1

	expected := `
# HELP wish_transaction_default_partition_rows Строк в партиции транзакций по умолчанию
# TYPE wish_transaction_default_partition_rows gauge
wish_transaction_default_partition_rows{service="test-partition"} -1
`
	if err := testutil.GatherAndCompare(prometheus.DefaultGatherer, strings.NewReader(expected),
		"wish_transaction_default_partition_rows"); err != nil {
		t.Error(err)
	}
}
