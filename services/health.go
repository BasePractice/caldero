package services

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// healthCheckTimeout ограничивает проверку зависимости: проба, которая
// висит, хуже пробы, которая отвечает отказом.
const healthCheckTimeout = 2 * time.Second

// Probe проверяет доступность зависимости.
type Probe func(ctx context.Context) error

// Health собирает пробы готовности сервиса.
type Health struct {
	mu     sync.RWMutex
	probes map[string]Probe
}

func NewHealth() *Health {
	return &Health{probes: make(map[string]Probe)}
}

// Register добавляет проверку зависимости.
func (h *Health) Register(name string, probe Probe) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.probes[name] = probe
}

// Handler отдаёт две пробы.
//
// Разделение существенно: liveness отвечает «процесс жив», и по её отказу
// оркестратор перезапускает контейнер. Readiness отвечает «сервис может
// обслуживать запросы», и по её отказу его лишь выводят из балансировки.
// Если проверять зависимости в liveness, недоступная база вызовет
// бесконечный перезапуск вместо ожидания.
func (h *Health) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), healthCheckTimeout)
		defer cancel()

		h.mu.RLock()
		probes := make(map[string]Probe, len(h.probes))
		for name, probe := range h.probes {
			probes[name] = probe
		}
		h.mu.RUnlock()

		// Пустой набор проверок означает, что сервис ещё не закончил
		// запуск: служебный порт поднимается раньше, чем регистрируются
		// зависимости. Отвечать «готов» в этот момент нельзя.
		if len(probes) == 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			if err := json.NewEncoder(w).Encode(map[string]string{"status": "starting"}); err != nil {
				slog.ErrorContext(ctx, "Encoding readiness", slog.String("err", err.Error()))
			}
			return
		}

		results := make(map[string]string, len(probes))
		ready := true
		for name, probe := range probes {
			if err := probe(ctx); err != nil {
				slog.DebugContext(ctx, "Readiness probe failed",
					slog.String("probe", name), slog.String("err", err.Error()))
				// Текст ошибки наружу не отдаётся: проба доступна без
				// аутентификации, а сообщения БД раскрывают внутренности.
				results[name] = "failed"
				ready = false
				continue
			}
			results[name] = "ok"
		}

		w.Header().Set("Content-Type", "application/json")
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		if err := json.NewEncoder(w).Encode(results); err != nil {
			slog.ErrorContext(ctx, "Encoding readiness", slog.String("err", err.Error()))
		}
	})

	return mux
}
