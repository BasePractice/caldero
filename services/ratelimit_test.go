package services

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestLimitByAddress проверяет то, ради чего предел и нужен: с одного
// адреса пропускается заданное число попыток, остальные отклоняются,
// а другой адрес это не задевает.
func TestLimitByAddress(t *testing.T) {
	passed := 0
	handler := LimitByAddress(2, time.Minute, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		passed++
	}))

	send := func(address string) int {
		request := httptest.NewRequest(http.MethodPost, "/auth", nil)
		request.RemoteAddr = address + ":50000"
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Code
	}

	if code := send("10.0.0.1"); code != http.StatusOK {
		t.Fatalf("первая попытка: код %d", code)
	}
	if code := send("10.0.0.1"); code != http.StatusOK {
		t.Fatalf("вторая попытка: код %d", code)
	}
	if code := send("10.0.0.1"); code != http.StatusTooManyRequests {
		t.Errorf("третья попытка: код %d, ожидался 429", code)
	}
	// Перебор с одного адреса не должен закрывать вход всем остальным.
	if code := send("10.0.0.2"); code != http.StatusOK {
		t.Errorf("другой адрес: код %d", code)
	}
	if passed != 3 {
		t.Errorf("до обработчика дошло %d запросов, ожидалось 3", passed)
	}
}

// TestLimitByAddressWindow: окно кончилось — счёт начинается заново.
// Иначе первый же перебор закрывал бы вход с этого адреса навсегда.
func TestLimitByAddressWindow(t *testing.T) {
	limiter := &addressLimiter{limit: 1, window: time.Minute, seen: make(map[string]*addressCounter)}
	start := time.Now()

	if !limiter.allow("10.0.0.1", start) {
		t.Fatal("первая попытка отклонена")
	}
	if limiter.allow("10.0.0.1", start.Add(time.Second)) {
		t.Error("вторая попытка в том же окне пропущена")
	}
	if !limiter.allow("10.0.0.1", start.Add(2*time.Minute)) {
		t.Error("попытка в новом окне отклонена")
	}
	// Истёкшие записи не должны копиться: иначе карта растёт по числу
	// адресов, которые когда-либо обращались.
	limiter.allow("10.0.0.2", start.Add(4*time.Minute))
	if len(limiter.seen) != 1 {
		t.Errorf("в памяти %d записей, ожидалась одна", len(limiter.seen))
	}
}

// TestLimitByAddressBehindProxy: сервис стоит за прокси, и без учёта
// X-Forwarded-For все запросы выглядели бы приходящими с одного адреса —
// самого прокси, — а предел закрывал бы вход всем сразу.
func TestLimitByAddressBehindProxy(t *testing.T) {
	handler := LimitByAddress(1, time.Minute, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	send := func(forwarded string) int {
		request := httptest.NewRequest(http.MethodPost, "/auth", nil)
		request.RemoteAddr = "10.0.0.9:50000"
		request.Header.Set("X-Forwarded-For", forwarded)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Code
	}

	if code := send("203.0.113.1"); code != http.StatusOK {
		t.Fatalf("первый клиент: код %d", code)
	}
	if code := send("203.0.113.1, 10.0.0.9"); code != http.StatusTooManyRequests {
		t.Errorf("тот же клиент через цепочку прокси: код %d, ожидался 429", code)
	}
	if code := send("203.0.113.2"); code != http.StatusOK {
		t.Errorf("другой клиент: код %d", code)
	}
}

// TestLimitByAddressDisabled: нулевой предел не должен ничего отклонять —
// так его выключают на стенде.
func TestLimitByAddressDisabled(t *testing.T) {
	handler := LimitByAddress(0, time.Minute, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	for range 5 {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/auth", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("код %d при выключенном пределе", recorder.Code)
		}
	}
}
