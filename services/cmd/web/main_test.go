package main

import (
	"embed"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wish/services"
)

//go:embed static/index.html static/wasm_exec.js
var testStatic embed.FS

func testHandler(t *testing.T) http.Handler {
	t.Helper()

	content, err := fs.Sub(testStatic, "static")
	if err != nil {
		t.Fatalf("подготовка статики: %v", err)
	}
	return handler(content, services.Config{
		WebAPIBase:  "http://localhost:8080/api/v1",
		WebClientId: "web",
	})
}

func TestConfigIsServed(t *testing.T) {
	recorder := httptest.NewRecorder()
	testHandler(t).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/config.json", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("код ответа %d", recorder.Code)
	}
	body := recorder.Body.String()
	// Адрес API отдаётся отдельным ответом, а не зашивается в бандл:
	// один и тот же файл должен работать и на стенде, и в бою.
	if !strings.Contains(body, "http://localhost:8080/api/v1") {
		t.Errorf("в настройках нет адреса API: %s", body)
	}
	if !strings.Contains(body, `"client_id":"web"`) {
		t.Errorf("в настройках нет клиента: %s", body)
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Error("настройки закэшированы")
	}
}

func TestIndexIsServed(t *testing.T) {
	recorder := httptest.NewRecorder()
	testHandler(t).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("код ответа %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "app.wasm") {
		t.Error("страница не загружает приложение")
	}
	// Разметка меняется вместе с приложением, а имя бандла постоянно:
	// закэшированный index.html показывал бы старый экран после выката.
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("разметка кэшируется: %q", recorder.Header().Get("Cache-Control"))
	}
}

func TestBundleIsCacheable(t *testing.T) {
	recorder := httptest.NewRecorder()
	testHandler(t).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/wasm_exec.js", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("код ответа %d", recorder.Code)
	}
	// Имя бандла постоянно, а содержимое меняется вместе с выкатом,
	// поэтому кэш короткий, но он есть: файл на несколько мегабайт
	// незачем тянуть на каждый переход.
	if cache := recorder.Header().Get("Cache-Control"); !strings.Contains(cache, "max-age") {
		t.Errorf("бандл не кэшируется: %q", cache)
	}
}

func TestMissingFile(t *testing.T) {
	recorder := httptest.NewRecorder()
	testHandler(t).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/missing.html", nil))

	if recorder.Code != http.StatusNotFound {
		t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusNotFound)
	}
}
