package services

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// tracingShutdownTimeout ограничивает выгрузку накопленных трасс при остановке.
const tracingShutdownTimeout = 5 * time.Second

// InitTracing создаёт TracerProvider и возвращает функцию остановки.
//
// Раньше инструментирован был только Redis-клиент, но провайдер не создавался
// нигде: библиотека писала трассы в no-op и они никуда не уходили. Jaeger
// получал данные исключительно от шлюза, поэтому сквозного пути запроса
// не было видно.
func InitTracing(ctx context.Context, service string, cfg Config) (func(context.Context), error) {
	if cfg.OTelEndpoint == "" {
		slog.Info("Tracing disabled: OTEL_EXPORTER_OTLP_ENDPOINT is not set")
		return func(context.Context) {}, nil
	}

	// Адрес не передаётся опцией намеренно: экспортёр сам читает
	// OTEL_EXPORTER_OTLP_ENDPOINT по спецификации и дописывает к нему
	// путь /v1/traces. Передача базового URL опцией этот путь теряет.
	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating otlp exporter: %w", err)
	}

	version, revision := BuildInfo()
	// Версия semconv обязана совпадать с той, что использует resource.Default():
	// иначе слияние падает с conflicting Schema URL.
	attributes, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(service),
		semconv.ServiceVersion(version),
		semconv.ServiceInstanceID(revision),
	))
	if err != nil {
		return nil, fmt.Errorf("building trace resource: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(attributes),
		// Доля трасс задаётся конфигурацией: сто процентов на нагруженном
		// сервисе — это лишний трафик и место в хранилище.
		sdktrace.WithSampler(sdktrace.ParentBased(
			sdktrace.TraceIDRatioBased(cfg.OTelSampleRatio))),
	)
	otel.SetTracerProvider(provider)

	// Без этого контекст трассы не переносится между сервисами, и вместо
	// одной сквозной трассы получаются отдельные несвязанные.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	slog.Info("Tracing enabled",
		slog.String("endpoint", cfg.OTelEndpoint),
		slog.Float64("sample_ratio", cfg.OTelSampleRatio))

	return func(shutdownCtx context.Context) {
		// Контекст остановки отдельный: родительский уже отменён сигналом,
		// а накопленные трассы нужно успеть выгрузить.
		timeoutCtx, cancel := context.WithTimeout(
			context.WithoutCancel(shutdownCtx), tracingShutdownTimeout)
		defer cancel()
		if err := provider.Shutdown(timeoutCtx); err != nil {
			slog.Error("Can't flush traces", slog.String("err", err.Error()))
		}
	}, nil
}
