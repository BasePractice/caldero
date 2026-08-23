package payment

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// Заголовки вебхука по умолчанию. У каждого провайдера они свои,
// поэтому имена вынесены в поля обработчика.
const (
	DefaultSignatureHeader = "X-Payment-Signature"
	DefaultTimestampHeader = "X-Payment-Timestamp"
	// MaxWebhookBody ограничивает тело вебхука: без ограничения
	// внешняя сторона задаёт объём памяти, который занимает наш процесс.
	MaxWebhookBody = 64 << 10
)

// WebhookHandler принимает вебхуки провайдера.
type WebhookHandler struct {
	verifier *Verifier
	store    OperationStore

	// SignatureHeader и TimestampHeader — где искать подпись и время.
	SignatureHeader string
	TimestampHeader string
	// ParseTimestamp разбирает значение заголовка времени.
	// По умолчанию — секунды epoch.
	ParseTimestamp func(string) (time.Time, error)
	// ParseEvent разбирает тело. По умолчанию — JSON в формате Event;
	// у провайдера формат свой, и подменяется он здесь.
	ParseEvent func([]byte) (Event, error)
	// MaxBody ограничивает размер тела.
	MaxBody int64
}

func NewWebhookHandler(verifier *Verifier, store OperationStore) *WebhookHandler {
	return &WebhookHandler{
		verifier:        verifier,
		store:           store,
		SignatureHeader: DefaultSignatureHeader,
		TimestampHeader: DefaultTimestampHeader,
		ParseTimestamp:  parseUnixTimestamp,
		ParseEvent:      parseJSONEvent,
		MaxBody:         MaxWebhookBody,
	}
}

func (h *WebhookHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	ctx := request.Context()
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, h.MaxBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(writer, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		slog.WarnContext(ctx, "Can't read webhook body", slog.String("err", err.Error()))
		http.Error(writer, "can't read body", http.StatusBadRequest)
		return
	}

	timestamp, err := h.ParseTimestamp(request.Header.Get(h.TimestampHeader))
	if err != nil {
		slog.WarnContext(ctx, "Malformed webhook timestamp", slog.String("err", err.Error()))
		http.Error(writer, "malformed timestamp", http.StatusBadRequest)
		return
	}
	if err = h.verifier.Verify(body, timestamp, request.Header.Get(h.SignatureHeader)); err != nil {
		// Тело не логируется: в нём данные платежа.
		slog.WarnContext(ctx, "Rejected webhook", slog.String("err", err.Error()))
		http.Error(writer, "signature mismatch", http.StatusBadRequest)
		return
	}

	event, err := h.ParseEvent(body)
	if err != nil {
		slog.WarnContext(ctx, "Malformed webhook event", slog.String("err", err.Error()))
		http.Error(writer, "malformed event", http.StatusBadRequest)
		return
	}
	if err = event.Validate(); err != nil {
		slog.WarnContext(ctx, "Invalid webhook event", slog.String("err", err.Error()))
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}

	operation, err := h.store.Apply(ctx, event, event.ApplyTo)
	switch {
	case errors.Is(err, ErrEventIgnored):
		// Повтор и опоздавшее событие — успех для провайдера: ответь он
		// ошибкой, провайдер повторял бы доставку до конца срока ретраев.
		slog.DebugContext(ctx, "Webhook event ignored",
			slog.String("event", event.Id), slog.String("err", err.Error()))
		writer.WriteHeader(http.StatusOK)
		return
	case errors.Is(err, ErrNotFound):
		// Операции нет: событие могло опередить запись о ней. Отвечаем
		// ошибкой, чтобы провайдер повторил доставку.
		slog.WarnContext(ctx, "Webhook for unknown operation",
			slog.String("operation", event.OperationId))
		http.Error(writer, "unknown operation", http.StatusServiceUnavailable)
		return
	case err != nil:
		// Сбой на нашей стороне: провайдер должен повторить, иначе
		// операция навсегда останется незавершённой.
		slog.ErrorContext(ctx, "Can't apply webhook event",
			slog.String("event", event.Id), slog.String("err", err.Error()))
		http.Error(writer, "can't apply event", http.StatusServiceUnavailable)
		return
	}

	slog.InfoContext(ctx, "Payment operation updated",
		slog.String("operation", operation.Id),
		slog.String("status", string(operation.Status)))
	writer.WriteHeader(http.StatusOK)
}

func parseUnixTimestamp(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, errors.New("timestamp header is empty")
	}
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing timestamp %q: %w", value, err)
	}
	return time.Unix(seconds, 0), nil
}

func parseJSONEvent(body []byte) (Event, error) {
	var event Event
	if err := json.Unmarshal(body, &event); err != nil {
		return Event{}, fmt.Errorf("decoding event: %w", err)
	}
	return event, nil
}
