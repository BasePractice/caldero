package services

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestDefineLogging(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"текстовый вывод", Config{LogLevel: "INFO"}},
		{"цветной вывод", Config{LogLevel: "DEBUG", LogColor: true}},
		{"уровень в нижнем регистре", Config{LogLevel: "warn"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logger, err := DefineLogging(test.cfg)
			if err != nil {
				t.Fatalf("настройка журнала: %v", err)
			}
			if logger == nil {
				t.Fatal("журнал не создан")
			}
		})
	}
}

// TestDefineLoggingFile: запись идёт и в файл, и в stdout — файл читает
// сборщик логов, а stdout нужен, чтобы docker compose logs продолжал работать.
func TestDefineLoggingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")

	logger, err := DefineLogging(Config{LogLevel: "INFO", LogFile: path})
	if err != nil {
		t.Fatalf("настройка журнала: %v", err)
	}
	logger.Info("проверка записи", slog.String("метка", "значение"))

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("чтение файла журнала: %v", err)
	}
	if !strings.Contains(string(content), "проверка записи") {
		t.Errorf("запись не попала в файл: %s", content)
	}
}

func TestDefineLoggingErrors(t *testing.T) {
	t.Run("неизвестный уровень", func(t *testing.T) {
		if _, err := DefineLogging(Config{LogLevel: "ГРОМКО"}); err == nil {
			t.Fatal("неизвестный уровень принят")
		}
	})

	t.Run("недоступный файл", func(t *testing.T) {
		// Каталог вместо файла: открыть его на запись нельзя, и такая
		// ошибка обязана останавливать старт.
		if _, err := DefineLogging(Config{LogLevel: "INFO", LogFile: t.TempDir()}); err == nil {
			t.Fatal("каталог принят как файл журнала")
		}
	})
}

// recordingHandler запоминает атрибуты записи: проверять формат вывода
// текстового обработчика бессмысленно, а состав атрибутов — это как раз
// то, ради чего traceHandler существует.
type recordingHandler struct {
	attrs  []slog.Attr
	groups []string
	last   slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, record slog.Record) error {
	h.last = record
	return nil
}

func (h *recordingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h.attrs = append(h.attrs, attrs...)
	return h
}

func (h *recordingHandler) WithGroup(name string) slog.Handler {
	h.groups = append(h.groups, name)
	return h
}

// TestTraceHandlerAddsIds фиксирует связь журнала и трассы: без этих полей
// по записи нельзя перейти к тому, что происходило в том же запросе.
func TestTraceHandlerAddsIds(t *testing.T) {
	inner := &recordingHandler{}
	handler := traceHandler{inner}

	traceId, err := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	if err != nil {
		t.Fatalf("идентификатор трассы: %v", err)
	}
	spanId, err := trace.SpanIDFromHex("0102030405060708")
	if err != nil {
		t.Fatalf("идентификатор спана: %v", err)
	}
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(
		trace.SpanContextConfig{TraceID: traceId, SpanID: spanId, TraceFlags: trace.FlagsSampled}))

	if err := handler.Handle(ctx, slog.Record{}); err != nil {
		t.Fatalf("обработка записи: %v", err)
	}

	found := map[string]string{}
	inner.last.Attrs(func(attr slog.Attr) bool {
		found[attr.Key] = attr.Value.String()
		return true
	})
	if found["trace_id"] != traceId.String() {
		t.Errorf("trace_id %q, ожидался %q", found["trace_id"], traceId)
	}
	if found["span_id"] != spanId.String() {
		t.Errorf("span_id %q, ожидался %q", found["span_id"], spanId)
	}
}

// TestTraceHandlerWithoutSpan: запись вне запроса не должна получать
// пустые идентификаторы — они только мешают при поиске.
func TestTraceHandlerWithoutSpan(t *testing.T) {
	inner := &recordingHandler{}
	handler := traceHandler{inner}

	if err := handler.Handle(context.Background(), slog.Record{}); err != nil {
		t.Fatalf("обработка записи: %v", err)
	}
	inner.last.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "trace_id" || attr.Key == "span_id" {
			t.Errorf("запись вне трассы получила %s", attr.Key)
		}
		return true
	})
}

// TestTraceHandlerWraps: WithAttrs и WithGroup обязаны возвращать снова
// traceHandler, иначе logger.With() тихо теряет идентификаторы трассы.
func TestTraceHandlerWraps(t *testing.T) {
	handler := traceHandler{&recordingHandler{}}

	if _, ok := handler.WithAttrs([]slog.Attr{slog.String("k", "v")}).(traceHandler); !ok {
		t.Error("WithAttrs вернул обработчик без поддержки трассы")
	}
	if _, ok := handler.WithGroup("группа").(traceHandler); !ok {
		t.Error("WithGroup вернул обработчик без поддержки трассы")
	}
}
