package services

import (
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LimitByAddress ограничивает частоту запросов с одного адреса.
//
// Нужен там, где запрос принимает пароль или код: без предела страница
// входа — это место для перебора, и перебор идёт ровно с той скоростью,
// с какой отвечает сервис.
//
// Счётчик в памяти процесса, а не в Redis: предел здесь защищает от
// перебора, а не считает квоту. Экземпляр сервиса с собственным счётчиком
// пропустит в N раз больше при N экземплярах — но и это на порядки меньше,
// чем без предела вовсе, а общее состояние стоило бы обращения в Redis
// на каждый запрос к странице входа.
//
// Нулевой предел выключает ограничение: так его отключают на стенде,
// где перебирать некому.
func LimitByAddress(limit int, window time.Duration, next http.Handler) http.Handler {
	if limit <= 0 || window <= 0 {
		return next
	}
	limiter := &addressLimiter{
		limit:  limit,
		window: window,
		seen:   make(map[string]*addressCounter),
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		address := clientAddress(r)
		if limiter.allow(address, time.Now()) {
			next.ServeHTTP(w, r)
			return
		}
		slog.WarnContext(r.Context(), "Request rejected: too many attempts from address",
			slog.String("path", r.URL.Path))
		w.Header().Set("Retry-After", strconv.Itoa(int(window.Seconds())))
		http.Error(w, "Too many requests", http.StatusTooManyRequests)
	})
}

type addressCounter struct {
	count int
	until time.Time
}

type addressLimiter struct {
	limit  int
	window time.Duration

	mu   sync.Mutex
	seen map[string]*addressCounter
}

// allow считает запросы окнами фиксированной длины: скользящее окно точнее,
// но требует хранить время каждого запроса, а разница здесь — не больше
// двойного предела на границе окон.
func (l *addressLimiter) allow(address string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	counter, ok := l.seen[address]
	if !ok || now.After(counter.until) {
		// Заодно убираются истёкшие записи: без этого карта растёт
		// по числу адресов, которые когда-либо обращались.
		l.sweep(now)
		l.seen[address] = &addressCounter{count: 1, until: now.Add(l.window)}
		return true
	}
	if counter.count >= l.limit {
		return false
	}
	counter.count++
	return true
}

func (l *addressLimiter) sweep(now time.Time) {
	for address, counter := range l.seen {
		if now.After(counter.until) {
			delete(l.seen, address)
		}
	}
}

// clientAddress определяет адрес клиента.
//
// X-Forwarded-For принимается только от прокси: сервис стоит за ним,
// и без учёта заголовка все запросы выглядели бы приходящими с одного
// адреса — самого прокси. Берётся первый адрес списка: его подставляет
// ближайший к клиенту прокси, а всё, что клиент прислал сам, идёт следом.
func clientAddress(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		first, _, _ := strings.Cut(forwarded, ",")
		return strings.TrimSpace(first)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
