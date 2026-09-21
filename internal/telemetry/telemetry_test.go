package telemetry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	collector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestShutdownExportsToConfiguredCollector(t *testing.T) {
	requests := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/collector/v1/traces" {
			t.Errorf("unexpected collector path: %s", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		requests <- body
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", server.URL+"/collector")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_COMPRESSION", "none")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_COMPRESSION", "none")
	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	defer otel.SetTracerProvider(previousProvider)
	defer otel.SetTextMapPropagator(previousPropagator)
	shutdown, err := Setup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, span := Tracer().Start(context.Background(), "synthetic-smoke")
	span.End()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case body := <-requests:
		var data collector.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &data); err != nil {
			t.Fatal(err)
		}
		if len(data.ResourceSpans) != 1 || len(data.ResourceSpans[0].ScopeSpans) != 1 {
			t.Fatal("missing exported resource/scope spans")
		}
		spans := data.ResourceSpans[0].ScopeSpans[0].Spans
		if len(spans) != 1 || spans[0].Name != "synthetic-smoke" {
			t.Fatal("shutdown did not flush the span")
		}
	case <-ctx.Done():
		t.Fatal("collector did not receive the trace")
	}
}
