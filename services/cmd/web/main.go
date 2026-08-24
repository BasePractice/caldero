package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"wish/services"
)

//go:embed static
var static embed.FS

var (
	port        = flag.Int("port", 51058, "The service port")
	healthcheck = flag.Bool("healthcheck", false, "Check that the service accepts connections and exit")
)

// appWasm — имя собранного приложения. Оно не лежит в репозитории:
// это артефакт сборки на пять мегабайт.
const appWasm = "app.wasm"

const (
	// loginAttempts и loginWindow ограничивают частоту обращений к странице
	// входа с одного адреса: она принимает пароль, и без предела это место
	// для перебора.
	loginAttempts = 20
	loginWindow   = time.Minute
)

func main() {
	flag.Parse()
	if *healthcheck {
		if err := services.Healthcheck(fmt.Sprintf("localhost:%d", *port)); err != nil {
			fmt.Fprintln(os.Stderr, "healthcheck failed:", err)
			os.Exit(1)
		}
		return
	}
	services.Run("web", func(ctx context.Context, cfg services.Config, health *services.Health) error {
		content, err := fs.Sub(static, "static")
		if err != nil {
			return fmt.Errorf("opening static content: %w", err)
		}

		// Проба готовности: без единой пробы readyz отвечает «ещё
		// запускаюсь» и не меняет ответ никогда — а раздача статики
		// готова ровно тогда, когда открыта сама статика.
		health.Register("static", func(ctx context.Context) error {
			if _, err := fs.Stat(content, "index.html"); err != nil {
				return fmt.Errorf("static content is not readable: %w", err)
			}
			return nil
		})

		if _, err = fs.Stat(content, appWasm); err != nil {
			// Не ошибка старта: страница откроется и скажет, что делать.
			// Падать здесь значило бы ронять раздачу статики из-за
			// несобранного фронтенда.
			slog.Warn("Application bundle is missing, run `make wasm`",
				slog.String("file", appWasm))
		}

		// Страница входа приходит от сервиса пользователей, но открывается
		// с адреса интерфейса: см. authProxy.
		auth, err := authProxy(cfg)
		if err != nil {
			return err
		}

		return services.ServeHTTP(ctx, cfg, fmt.Sprintf(":%d", *port), handler(content, cfg, auth))
	})
}

// authProxy пробрасывает страницу входа к сервису пользователей.
//
// Она не идёт через шлюз намеренно. Вход заканчивается перенаправлением
// на redirect_uri с кодом, а KrakenD Community Edition сам ходит
// по перенаправлению бэкенда и отдаёт клиенту тело того, что нашёл там:
// код авторизации до браузера не доезжает вовсе (EXT-10). Отключается это
// только в платной редакции.
//
// Адрес интерфейса для страницы входа — то, что нужно и без того: адрес
// возврата клиента совпадает с origin приложения, а браузер остаётся
// на одном источнике весь вход.
//
// Пустой USERS_ENDPOINT выключает маршрут: раздача статики от него
// не зависит, и падать из-за него незачем.
func authProxy(cfg services.Config) (http.Handler, error) {
	if cfg.UsersEndpoint == "" {
		slog.Warn("Login page is not available: USERS_ENDPOINT is not set")
		return nil, nil
	}

	target, err := url.Parse(cfg.UsersEndpoint)
	if err != nil {
		return nil, fmt.Errorf("parsing users endpoint %q: %w", cfg.UsersEndpoint, err)
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			// Путь сохраняется как есть: у сервиса пользователей он
			// такой же, а SetURL иначе приклеил бы путь адреса сервиса.
			r.Out.URL.Path = r.In.URL.Path
			r.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.ErrorContext(r.Context(), "Login page is unavailable",
				slog.String("err", err.Error()))
			http.Error(w, "Login is temporarily unavailable", http.StatusBadGateway)
		},
	}
	return services.LimitByAddress(loginAttempts, loginWindow, proxy), nil
}

// handler раздаёт статику, подставляет адрес API и отдаёт страницу входа.
func handler(content fs.FS, cfg services.Config, auth http.Handler) http.Handler {
	files := http.FileServer(http.FS(content))

	mux := http.NewServeMux()
	if auth != nil {
		// Страница входа, отправка её формы и возврат от внешнего
		// провайдера: все три ответа — перенаправления, и пройти они
		// должны нетронутыми. Методы указаны явно: с раздачей статики
		// (GET /) шаблон без метода несовместим.
		mux.Handle("GET /auth", auth)
		mux.Handle("POST /auth", auth)
		mux.Handle("GET /auth/", auth)
	}
	// Адрес API отдаётся отдельным ответом, а не зашивается в бандл:
	// один и тот же app.wasm должен работать и на стенде, и в бою.
	mux.HandleFunc("GET /config.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		// Ответ уже начат: сообщить об ошибке записи клиенту нечем.
		_, _ = fmt.Fprintf(w, `{"api":%q,"client_id":%q}`+"\n",
			cfg.WebAPIBase, cfg.WebClientId)
	})
	mux.Handle("GET /", cacheControl(files))
	return services.Measure("web", mux)
}

// cacheControl запрещает кэшировать разметку и разрешает — бандл.
//
// Разметка меняется вместе с приложением, а имя бандла постоянно,
// поэтому браузер, закэшировавший index.html, показывал бы старый экран
// после каждого выката.
func cacheControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".wasm") || strings.HasSuffix(r.URL.Path, ".js") {
			w.Header().Set("Cache-Control", "public, max-age=300")
		} else {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}
