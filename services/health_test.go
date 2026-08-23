package services

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func probeResult(t *testing.T, handler http.Handler, path string) (*httptest.ResponseRecorder, map[string]string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))

	if recorder.Body.Len() == 0 {
		return recorder, nil
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		return recorder, nil
	}
	return recorder, body
}

// TestLivenessIgnoresDependencies фиксирует разделение проб: liveness отвечает
// «процесс жив» и не смотрит на зависимости. Иначе недоступная база вызывает
// бесконечный перезапуск контейнера вместо вывода из балансировки.
func TestLivenessIgnoresDependencies(t *testing.T) {
	health := NewHealth()
	health.Register("database", func(context.Context) error {
		return errors.New("connection refused")
	})

	recorder, _ := probeResult(t, health.Handler(), "/livez")
	if recorder.Code != http.StatusOK {
		t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusOK)
	}
	if recorder.Body.String() != "ok" {
		t.Errorf("тело %q, ожидалось ok", recorder.Body)
	}
}

// TestReadinessWithoutProbes: служебный порт поднимается раньше, чем
// регистрируются зависимости, и до их регистрации сервис не готов.
func TestReadinessWithoutProbes(t *testing.T) {
	recorder, body := probeResult(t, NewHealth().Handler(), "/readyz")

	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if body["status"] != "starting" {
		t.Errorf("тело %v, ожидался признак запуска", body)
	}
}

func TestReadinessReady(t *testing.T) {
	health := NewHealth()
	health.Register("database", func(context.Context) error { return nil })
	health.Register("cache", func(context.Context) error { return nil })

	recorder, body := probeResult(t, health.Handler(), "/readyz")
	if recorder.Code != http.StatusOK {
		t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusOK)
	}
	if body["database"] != "ok" || body["cache"] != "ok" {
		t.Errorf("тело %v, ожидались обе пробы в порядке", body)
	}
}

func TestReadinessNotReady(t *testing.T) {
	health := NewHealth()
	health.Register("database", func(context.Context) error {
		return errors.New("connection refused: password authentication failed for user postgres")
	})
	health.Register("cache", func(context.Context) error { return nil })

	recorder, body := probeResult(t, health.Handler(), "/readyz")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if body["database"] != "failed" || body["cache"] != "ok" {
		t.Errorf("тело %v, ожидался отказ одной пробы", body)
	}
	// Проба доступна без аутентификации, поэтому текст ошибки наружу
	// не отдаётся: сообщения БД раскрывают внутренности.
	if strings.Contains(recorder.Body.String(), "password") {
		t.Errorf("текст ошибки утёк наружу: %s", recorder.Body)
	}
}

// TestRegisterConcurrently: пробы регистрируются в main, а читаются
// обработчиком — гонка здесь возможна, и она проверяется под -race.
func TestRegisterConcurrently(t *testing.T) {
	health := NewHealth()
	handler := health.Handler()
	done := make(chan struct{})

	go func() {
		defer close(done)
		for i := range 50 {
			health.Register(string(rune('a'+i%26)), func(context.Context) error { return nil })
		}
	}()
	for range 50 {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	}
	<-done
}
