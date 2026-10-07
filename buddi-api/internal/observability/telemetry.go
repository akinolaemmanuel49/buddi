// Package observability sets up tracing and structured logging. When no OTLP
// endpoint is configured the exporters are not installed at all, so local
// development runs carry no telemetry overhead and no network dependency.
package observability

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Options configures the tracing pipeline.
type Options struct {
	ServiceName   string
	ServiceVer    string
	Environment   string
	Endpoint      string
	SampleRatio   float64
	Insecure      bool
	LogLevel      string
	ExportTimeout time.Duration
}

// Shutdown flushes pending spans. It is safe to call on a no-op pipeline.
type Shutdown func(context.Context) error

// Setup installs the global tracer provider and returns a slog logger whose
// records carry the active trace and span ids. Logs always go to stdout as
// JSON: unlike the OpenTelemetry log bridge, this cannot silently discard
// records when no log pipeline is installed. The returned function flushes and
// tears down the tracing pipeline.
func Setup(ctx context.Context, opts Options) (*slog.Logger, Shutdown, error) {
	if opts.ServiceName == "" {
		opts.ServiceName = "buddi-api"
	}

	if opts.ExportTimeout <= 0 {
		opts.ExportTimeout = 5 * time.Second
	}

	handler := TraceHandler{Handler: newJSONHandler(os.Stdout, parseLevel(opts.LogLevel))}

	if !opts.Enabled() {
		return slog.New(handler), noopShutdown, nil
	}

	exporterOpts := []otlptracehttp.Option{
		otlptracehttp.WithEndpointURL(tracesEndpoint(opts.Endpoint)),
		otlptracehttp.WithTimeout(opts.ExportTimeout),
	}

	// TLS is the default; the local collector almost always speaks plaintext,
	// so the flag exists to make the insecure choice explicit.
	if opts.Insecure {
		exporterOpts = append(exporterOpts, otlptracehttp.WithInsecure())
	}

	exporter, err := otlptracehttp.New(ctx, exporterOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("observability: exporter: %w", err)
	}

	sampler := sdktrace.AlwaysSample()
	if opts.SampleRatio > 0 && opts.SampleRatio < 1 {
		sampler = sdktrace.ParentBased(sdktrace.TraceIDRatioBased(opts.SampleRatio))
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes("", attributes(opts)...))
	if err != nil {
		return nil, nil, fmt.Errorf("observability: resource: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithSampler(sampler),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	log := slog.New(handler)

	shutdown := func(ctx context.Context) error {
		if err := provider.Shutdown(ctx); err != nil {
			return fmt.Errorf("observability: shutdown: %w", err)
		}

		return nil
	}

	return log, shutdown, nil
}

// Enabled reports whether an OTLP endpoint was configured.
func (o Options) Enabled() bool {
	return strings.TrimSpace(o.Endpoint) != ""
}

// tracesEndpoint turns the configured base endpoint into the concrete traces
// URL. otlptracehttp.WithEndpointURL takes the URL literally and does not append
// the signal path, so an endpoint of http://host:4318 would POST to "/" and be
// answered with 404. The OTLP convention is that the base endpoint has the signal
// appended, so do that here, and leave an endpoint that already names its path
// alone.
func tracesEndpoint(base string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(base), "/")

	parsed, err := url.Parse(trimmed)
	if err != nil || (parsed.Path != "" && parsed.Path != "/") {
		return trimmed
	}

	return trimmed + "/v1/traces"
}

// Instrument wraps handler so every inbound request produces a span, with the
// status set from the response code.
func Instrument(handler http.Handler, operation string) http.Handler {
	return otelhttp.NewHandler(handler, operation)
}

// TraceIDFromContext returns the current trace id, or an empty string when no
// span is active. It is stored on agent_runs so a run can be found in the
// tracing backend from the database.
func TraceIDFromContext(ctx context.Context) string {
	spanCtx := trace.SpanContextFromContext(ctx)
	if !spanCtx.IsValid() {
		return ""
	}

	return spanCtx.TraceID().String()
}

// MarkError records err on the active span and marks it as failed.
func MarkError(ctx context.Context, err error) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}

	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// TraceHandler decorates log records with the active trace and span ids, so a
// log line can be tied to the request or run that produced it without
// depending on an OpenTelemetry log pipeline.
type TraceHandler struct {
	slog.Handler
}

func (h TraceHandler) Handle(ctx context.Context, record slog.Record) error {
	if spanCtx := trace.SpanContextFromContext(ctx); spanCtx.IsValid() {
		record.AddAttrs(
			slog.String("trace_id", spanCtx.TraceID().String()),
			slog.String("span_id", spanCtx.SpanID().String()),
		)
	}

	return h.Handler.Handle(ctx, record)
}

func (h TraceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return TraceHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h TraceHandler) WithGroup(name string) slog.Handler {
	return TraceHandler{Handler: h.Handler.WithGroup(name)}
}

func attributes(opts Options) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("service.name", opts.ServiceName),
	}

	if opts.ServiceVer != "" {
		attrs = append(attrs, attribute.String("service.version", opts.ServiceVer))
	}

	if opts.Environment != "" {
		attrs = append(attrs, attribute.String("deployment.environment", opts.Environment))
	}

	return attrs
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// newJSONHandler builds the application's log handler.
//
// Durations are rendered as text rather than left as slog's integer nanosecond
// encoding, so the handler is built in one place and the tests can render through
// the same configuration Setup installs.
func newJSONHandler(w io.Writer, level slog.Level) slog.Handler {
	return slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: humaniseDurations,
	})
}

// humaniseDurations renders a time.Duration attribute as its string form.
//
// slog encodes a duration as an integer nanosecond count, so a configured lease
// appears in the log as 120000000000. Reading that back requires knowing that the
// value is nanoseconds at all, and a misread interval is a misconfigured worker.
// The value stays exact: Duration.String picks a unit and keeps the digits.
func humaniseDurations(_ []string, a slog.Attr) slog.Attr {
	if duration, ok := a.Value.Any().(time.Duration); ok {
		return slog.String(a.Key, duration.String())
	}

	return a
}

func noopShutdown(context.Context) error {
	return nil
}
