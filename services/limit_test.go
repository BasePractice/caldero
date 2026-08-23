package services

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestLimitInFlightRejectsExcess(t *testing.T) {
	const limit = 2

	release := make(chan struct{})
	entered := make(chan struct{}, limit)
	handler := LimitInFlight(limit, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	}))

	// Занимаем все слоты и держим их.
	var wg sync.WaitGroup
	for range limit {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
		}()
	}
	for range limit {
		<-entered
	}

	// Лишний запрос должен получить отказ немедленно, а не встать в очередь.
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("код %d, ожидался 503", recorder.Code)
	}
	if recorder.Header().Get("Retry-After") == "" {
		t.Error("нет заголовка Retry-After")
	}

	close(release)
	wg.Wait()

	// После освобождения слотов запросы снова проходят.
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusOK {
		t.Errorf("код %d, ожидался 200", recorder.Code)
	}
}

func TestLimitInFlightDisabled(t *testing.T) {
	handler := LimitInFlight(0, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusOK {
		t.Errorf("код %d, ожидался 200: нулевой предел должен снимать ограничение", recorder.Code)
	}
}

func TestHealthNotReadyBeforeProbes(t *testing.T) {
	health := NewHealth()
	handler := health.Handler()

	// Служебный порт поднимается раньше, чем регистрируются зависимости:
	// в этот момент отвечать «готов» нельзя.
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("код %d, ожидался 503 до регистрации проверок", recorder.Code)
	}

	// Liveness при этом отвечает: процесс жив, перезапускать его не нужно.
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if recorder.Code != http.StatusOK {
		t.Errorf("код %d, ожидался 200", recorder.Code)
	}
}
