package logx

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func capture(t *testing.T, level slog.Level) (*bytes.Buffer, *slog.Logger) {
	t.Helper()
	var buf bytes.Buffer
	lvl := level
	l := New(Options{Service: "test-svc", Level: &lvl, Writer: &buf,
		Secrets: []string{"super-secret-token"}})
	return &buf, l
}

func TestMaskInMessageAndAttrs(t *testing.T) {
	buf, l := capture(t, slog.LevelInfo)

	l.Info("auth failed", "token", "super-secret-token",
		slog.Group("nested", "url", "https://t.me/bot super-secret-token"))

	out := buf.String()
	if strings.Contains(out, "super-secret-token") {
		t.Fatalf("secret leaked: %s", out)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if rec["token"] != "***" {
		t.Errorf("attr not masked: %v", rec["token"])
	}
	if m, ok := rec["nested"].(map[string]any); !ok || m["url"] != "https://t.me/bot ***" {
		t.Errorf("nested group not masked: %v", rec["nested"])
	}
}

func TestEmptySecretIgnored(t *testing.T) {
	h := slog.NewJSONHandler(&bytes.Buffer{}, nil)
	if got := NewRedactHandler(h); got != slog.Handler(h) {
		t.Error("expected unchanged handler when no secrets given")
	}
	if got := NewRedactHandler(h, ""); got != slog.Handler(h) {
		t.Error("empty secret must be ignored")
	}
}

func TestRecordShape(t *testing.T) {
	buf, l := capture(t, slog.LevelInfo)
	l.Info("hello")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	for _, key := range []string{"time", "level", "msg", "service"} {
		if _, ok := rec[key]; !ok {
			t.Errorf("missing standard field %q in %v", key, rec)
		}
	}
	for _, key := range []string{"trace_id", "span_id", "trace_flags"} {
		if _, ok := rec[key]; ok {
			t.Errorf("trace field %q present without a span: %v", key, rec)
		}
	}
	if rec["level"] != "INFO" || rec["service"] != "test-svc" {
		t.Errorf("unexpected record: %v", rec)
	}
}

func TestLevelFiltering(t *testing.T) {
	buf, l := capture(t, slog.LevelWarn)
	l.Info("dropped")
	if buf.Len() != 0 {
		t.Errorf("info must be filtered at warn level: %s", buf.String())
	}
	l.Warn("kept")
	if !strings.Contains(buf.String(), `"msg":"kept"`) {
		t.Errorf("warn must pass: %s", buf.String())
	}
}

func TestSetupSetsDefault(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	l := Setup("setup-svc")
	if slog.Default() != l {
		t.Error("Setup must install the default logger")
	}
}

func TestLevelFromEnv(t *testing.T) {
	cases := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"":      slog.LevelInfo,
		"INFO":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"WARN":  slog.LevelWarn,
		"error": slog.LevelError,
	}
	for in, want := range cases {
		t.Setenv("LOG_LEVEL", in)
		got, err := levelFromEnv()
		if err != nil || got != want {
			t.Errorf("LOG_LEVEL=%q: got (%v, %v), want %v", in, got, err, want)
		}
	}
}

func TestTraceContextFields(t *testing.T) {
	buf, l := capture(t, slog.LevelInfo)
	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	if err != nil {
		t.Fatal(err)
	}
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	if err != nil {
		t.Fatal(err)
	}
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
		Remote:     false,
	}))

	l.InfoContext(ctx, "with trace")
	l.ErrorContext(ctx, "with trace error")

	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("bad json: %v", err)
		}
		if rec["trace_id"] != traceID.String() || rec["span_id"] != spanID.String() || rec["trace_flags"] != "01" {
			t.Errorf("unexpected trace fields: %v", rec)
		}
	}
}
