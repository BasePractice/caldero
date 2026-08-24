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
	return testHandlerWithAuth(t, nil)
}

func testHandlerWithAuth(t *testing.T, auth http.Handler) http.Handler {
	t.Helper()

	content, err := fs.Sub(testStatic, "static")
	if err != nil {
		t.Fatalf("подготовка статики: %v", err)
	}
	return handler(content, services.Config{
		WebAPIBase:  "http://localhost:8080/api/v1",
		WebClientId: "web",
	}, auth)
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

// TestLoginIsProxied: страница входа отдаётся с адреса интерфейса, а не
// через шлюз. Через шлюз она не работает вовсе — KrakenD сам ходит
// по перенаправлению и не отдаёт браузеру код авторизации (EXT-10).
func TestLoginIsProxied(t *testing.T) {
	var seen []string
	auth := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		// Ответ страницы входа — перенаправление; оно обязано дойти
		// до браузера нетронутым.
		http.Redirect(w, r, "http://localhost:3000/?code=x", http.StatusFound)
	})
	handler := testHandlerWithAuth(t, auth)

	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/auth?client_id=web", nil),
		httptest.NewRequest(http.MethodPost, "/auth?client_id=web", strings.NewReader("username=a")),
		httptest.NewRequest(http.MethodGet, "/auth/social/yandex", nil),
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusFound {
			t.Errorf("%s %s: код ответа %d, ожидалось перенаправление",
				request.Method, request.URL.Path, recorder.Code)
		}
		if location := recorder.Header().Get("Location"); location == "" {
			t.Errorf("%s %s: перенаправление без адреса", request.Method, request.URL.Path)
		}
	}

	want := []string{"GET /auth", "POST /auth", "GET /auth/social/yandex"}
	if len(seen) != len(want) {
		t.Fatalf("до сервиса пользователей дошло %v, ожидалось %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("запрос %d: %s, ожидался %s", i, seen[i], want[i])
		}
	}
}

// TestLoginIsOptional: без адреса сервиса пользователей страница входа
// недоступна, но раздача статики от этого не зависит.
func TestLoginIsOptional(t *testing.T) {
	recorder := httptest.NewRecorder()
	testHandler(t).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/auth", nil))

	// Без маршрута /auth попадает в раздачу статики: там такого файла нет.
	if recorder.Code != http.StatusNotFound {
		t.Errorf("код ответа %d, ожидался 404", recorder.Code)
	}
}

// TestAuthProxyReachesUsers: запрос уходит к сервису пользователей тем же
// путём, с каким пришёл, и ответ возвращается нетронутым.
func TestAuthProxyReachesUsers(t *testing.T) {
	var path, forwarded string
	users := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path + "?" + r.URL.RawQuery
		forwarded = r.Header.Get("X-Forwarded-For")
		http.Redirect(w, r, "http://localhost:3000/?code=x", http.StatusFound)
	}))
	t.Cleanup(users.Close)

	auth, err := authProxy(services.Config{UsersEndpoint: users.URL})
	if err != nil {
		t.Fatalf("подготовка маршрута входа: %v", err)
	}

	recorder := httptest.NewRecorder()
	auth.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/auth?client_id=web", nil))

	if recorder.Code != http.StatusFound {
		t.Fatalf("код ответа %d, ожидалось перенаправление", recorder.Code)
	}
	if path != "/auth?client_id=web" {
		t.Errorf("до сервиса дошло %q", path)
	}
	// Без X-Forwarded-For предел попыток считал бы все запросы
	// приходящими с одного адреса — самого сервиса интерфейса.
	if forwarded == "" {
		t.Error("адрес клиента не передан")
	}
}

// TestAuthProxyWithoutUsers: недоступный сервис пользователей — это отказ
// входа, а не пустая страница с непонятной ошибкой.
func TestAuthProxyWithoutUsers(t *testing.T) {
	users := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := users.URL
	users.Close()

	auth, err := authProxy(services.Config{UsersEndpoint: endpoint})
	if err != nil {
		t.Fatalf("подготовка маршрута входа: %v", err)
	}

	recorder := httptest.NewRecorder()
	auth.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/auth", nil))
	if recorder.Code != http.StatusBadGateway {
		t.Errorf("код ответа %d, ожидался 502", recorder.Code)
	}
}

// TestAuthProxyIsOptional: без адреса сервиса пользователей маршрут входа
// не поднимается, но раздача статики от этого не зависит.
func TestAuthProxyIsOptional(t *testing.T) {
	auth, err := authProxy(services.Config{})
	if err != nil {
		t.Fatalf("подготовка маршрута входа: %v", err)
	}
	if auth != nil {
		t.Error("маршрут входа поднялся без адреса сервиса пользователей")
	}

	if _, err = authProxy(services.Config{UsersEndpoint: "://"}); err == nil {
		t.Error("неразбираемый адрес сервиса принят")
	}
}
