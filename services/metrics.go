package services

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// RED-метрики: частота запросов, доля ошибок и распределение длительности.
// Единственной метрикой «сколько раз вызвали Create» ни один вопрос
// о работе сервиса не решается.
var (
	requestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wish_requests_total",
		Help: "Число обработанных запросов",
	}, []string{"service", "transport", "method", "route", "code"})

	requestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "wish_request_duration_seconds",
		Help:    "Длительность обработки запроса",
		Buckets: prometheus.DefBuckets,
	}, []string{"service", "transport", "method", "route"})
)

func init() {
	prometheus.MustRegister(requestsTotal, requestDuration)
}

// statusRecorder запоминает код ответа: ResponseWriter его не отдаёт.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(data)
}

// Measure снимает метрики по каждому запросу. Метка маршрута берётся из
// шаблона ServeMux, а не из пути: иначе идентификаторы в URL дали бы
// неограниченное число временных рядов.
func Measure(service string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w}

		next.ServeHTTP(recorder, r)

		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		if recorder.status == 0 {
			recorder.status = http.StatusOK
		}
		labels := prometheus.Labels{
			"service": service, "transport": "http",
			"method": r.Method, "route": route,
		}
		requestDuration.With(labels).Observe(time.Since(started).Seconds())
		labels["code"] = strconv.Itoa(recorder.status)
		requestsTotal.With(labels).Inc()
	})
}

// MeasureUnaryInterceptor — то же для gRPC.
func MeasureUnaryInterceptor(service string) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		started := time.Now()
		resp, err := handler(ctx, req)

		labels := prometheus.Labels{
			"service": service, "transport": "grpc",
			"method": "rpc", "route": info.FullMethod,
		}
		requestDuration.With(labels).Observe(time.Since(started).Seconds())
		labels["code"] = status.Code(err).String()
		requestsTotal.With(labels).Inc()
		return resp, err
	}
}

// StatsProvider отдаёт состояние пула соединений. Интерфейс объявлен здесь,
// у потребителя: репозиториям незачем знать про метрики.
type StatsProvider interface {
	Stats() sql.DBStats
}

// RegisterDefaultPartitionRows публикует число строк в партиции по умолчанию.
// Ненулевое значение означает, что окно партиций кончилось: вставки
// продолжают проходить, но данные снова копятся в одной таблице.
// Значение -1 означает, что подсчёт не удался.
func RegisterDefaultPartitionRows(service string, count func() int64) {
	prometheus.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name:        "wish_transaction_default_partition_rows",
		Help:        "Строк в партиции транзакций по умолчанию",
		ConstLabels: prometheus.Labels{"service": service},
	}, func() float64 { return float64(count()) }))
}

// RegisterDBStats публикует состояние пула соединений. Исчерпание пула
// выглядит как замедление сервиса, и без этих метрик причину не отличить
// от медленной базы.
func RegisterDBStats(service string, db StatsProvider) {
	prometheus.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name:        "wish_db_connections_open",
		Help:        "Открытых соединений с БД",
		ConstLabels: prometheus.Labels{"service": service},
	}, func() float64 { return float64(db.Stats().OpenConnections) }))

	prometheus.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name:        "wish_db_connections_in_use",
		Help:        "Соединений с БД, занятых запросами",
		ConstLabels: prometheus.Labels{"service": service},
	}, func() float64 { return float64(db.Stats().InUse) }))

	prometheus.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name:        "wish_db_wait_total",
		Help:        "Сколько раз запрос ждал свободного соединения",
		ConstLabels: prometheus.Labels{"service": service},
	}, func() float64 { return float64(db.Stats().WaitCount) }))
}
