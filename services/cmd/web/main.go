package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"

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

		if _, err = fs.Stat(content, appWasm); err != nil {
			// Не ошибка старта: страница откроется и скажет, что делать.
			// Падать здесь значило бы ронять раздачу статики из-за
			// несобранного фронтенда.
			slog.Warn("Application bundle is missing, run `make wasm`",
				slog.String("file", appWasm))
		}

		return services.ServeHTTP(ctx, cfg, fmt.Sprintf(":%d", *port), handler(content, cfg))
	})
}

// handler раздаёт статику и подставляет адрес API.
func handler(content fs.FS, cfg services.Config) http.Handler {
	files := http.FileServer(http.FS(content))

	mux := http.NewServeMux()
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
