// Package telemetry records metadata only: no prompts, tool args, PII or keys.
package telemetry

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type Config struct {
	Enabled  bool   `mapstructure:"enabled"`
	File     string `mapstructure:"file"`
	Endpoint string `mapstructure:"endpoint"`
}

func Init(ctx context.Context, c Config) (func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }
	if !c.Enabled {
		return noop, nil
	}
	var exporter sdktrace.SpanExporter
	var file *os.File
	var err error
	if c.Endpoint != "" {
		exporter, err = otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(c.Endpoint))
	} else {
		if c.File == "" {
			return nil, errors.New("telemetry requires a local file or OTLP endpoint")
		}
		if err = os.MkdirAll(filepath.Dir(c.File), 0700); err != nil {
			return nil, err
		}
		file, err = os.OpenFile(c.File, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		exporter, err = stdouttrace.New(stdouttrace.WithWriter(file))
	}
	if err != nil {
		if file != nil {
			file.Close()
		}
		return nil, err
	}
	p := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", "lanagent"))))
	otel.SetTracerProvider(p)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return func(ctx context.Context) error {
		err := p.Shutdown(ctx)
		if file != nil {
			err = errors.Join(err, file.Close())
		}
		return err
	}, nil
}
func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return otel.Tracer("lanagent").Start(ctx, name, trace.WithAttributes(attrs...))
}
func Finish(span trace.Span, err error) {
	if err != nil {
		span.RecordError(errors.New("operation failed"))
		span.SetStatus(codes.Error, "operation failed")
	}
	span.End()
}
func Capture(ctx context.Context) map[string]string {
	c := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, c)
	return c
}
func Restore(ctx context.Context, c map[string]string) context.Context {
	return propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier(c))
}
func HTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := propagation.TraceContext{}.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := Start(ctx, "http.request", attribute.String("http.request.method", r.Method))
		defer span.End()
		// No URL or query attributes: those can contain identifiers and secrets.
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
