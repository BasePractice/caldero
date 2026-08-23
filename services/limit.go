package services

import (
	"log/slog"
	"net/http"
)

// LimitInFlight ограничивает число одновременно обрабатываемых запросов.
//
// Без предела всплеск нагрузки превращается в неограниченный рост горутин
// и ожидающих соединений с БД: сервис не отказывает, а деградирует до
// состояния, когда не отвечает никому. Явный отказ лишним запросам лучше:
// клиент видит 503 и может повторить, а не ждёт неопределённо долго.
func LimitInFlight(limit int, next http.Handler) http.Handler {
	if limit <= 0 {
		return next
	}
	// Буферизованный канал как счётчик: занятость видна по его заполнению,
	// а неблокирующая отправка даёт мгновенный отказ вместо ожидания.
	slots := make(chan struct{}, limit)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
			next.ServeHTTP(w, r)
		default:
			slog.WarnContext(r.Context(), "Request rejected: too many in flight",
				slog.Int("limit", limit), slog.String("path", r.URL.Path))
			w.Header().Set("Retry-After", "1")
			http.Error(w, "Too many requests in flight", http.StatusServiceUnavailable)
		}
	})
}
