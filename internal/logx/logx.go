// Package logx provides the standard logging setup used across all projects:
// a JSON handler on stdout, level from LOG_LEVEL (default info), a "service"
// attribute on every record, and masking of secrets in messages and attributes.
package logx

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

const masked = "***"

// Options configures the logger returned by New.
type Options struct {
	// Service is added as the "service" attribute to every record.
	Service string
	// Level overrides the level. When nil, it is parsed from LOG_LEVEL
	// (debug|info|warn|error, case-insensitive; default info).
	Level *slog.Level
	// Writer receives the logs; defaults to os.Stdout.
	Writer io.Writer
	// Secrets are masked with "***" wherever they appear in log messages
	// or attribute values (including nested groups).
	Secrets []string
}

// New builds the standard JSON logger.
func New(opts Options) *slog.Logger {
	level := slog.LevelInfo
	if opts.Level != nil {
		level = *opts.Level
	} else if lvl, err := levelFromEnv(); err == nil {
		level = lvl
	}
	w := opts.Writer
	if w == nil {
		w = os.Stdout
	}
	var h slog.Handler = slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	// Service attr must be attached BEFORE the redact wrapper:
	// Handler.WithAttrs on the wrapper would delegate to the inner handler
	// and records would bypass masking.
	if opts.Service != "" {
		h = h.WithAttrs([]slog.Attr{slog.String("service", opts.Service)})
	}
	h = NewRedactHandler(h, opts.Secrets...)
	h = NewTraceHandler(h)
	return slog.New(h)
}

// Setup builds the standard logger for service and installs it as the
// default slog logger. Secrets are masked in every record.
func Setup(service string, secrets ...string) *slog.Logger {
	l := New(Options{Service: service, Secrets: secrets})
	slog.SetDefault(l)
	return l
}

func levelFromEnv() (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL"))) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, os.ErrInvalid
	}
}

// RedactHandler wraps a slog.Handler and masks occurrences of secrets in all
// logged messages, attribute values and nested groups.
type RedactHandler struct {
	slog.Handler
	secrets []string
}

// NewRedactHandler wraps h so that any occurrence of secrets in log records
// is replaced with "***". If no non-empty secrets are given, h is returned
// unchanged.
func NewRedactHandler(h slog.Handler, secrets ...string) slog.Handler {
	filtered := make([]string, 0, len(secrets))
	for _, s := range secrets {
		if s != "" {
			filtered = append(filtered, s)
		}
	}
	if len(filtered) == 0 {
		return h
	}
	return &RedactHandler{Handler: h, secrets: filtered}
}

func (h *RedactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &RedactHandler{Handler: h.Handler.WithAttrs(attrs), secrets: h.secrets}
}

func (h *RedactHandler) WithGroup(name string) slog.Handler {
	return &RedactHandler{Handler: h.Handler.WithGroup(name), secrets: h.secrets}
}

func (h *RedactHandler) Handle(ctx context.Context, r slog.Record) error {
	redacted := slog.NewRecord(r.Time, r.Level, mask(r.Message, h.secrets), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		redacted.AddAttrs(h.redactAttr(a))
		return true
	})
	return h.Handler.Handle(ctx, redacted)
}

func (h *RedactHandler) redactAttr(a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindGroup {
		children := a.Value.Group()
		for i := range children {
			children[i] = h.redactAttr(children[i])
		}
		a.Value = slog.GroupValue(children...)
		return a
	}
	a.Value = slog.StringValue(mask(a.Value.String(), h.secrets))
	return a
}

func mask(s string, secrets []string) string {
	for _, sec := range secrets {
		s = strings.ReplaceAll(s, sec, masked)
	}
	return s
}

// TraceHandler adds the active OpenTelemetry span context to each record.
type TraceHandler struct {
	slog.Handler
}

// NewTraceHandler wraps h and adds trace_id, span_id, and trace_flags when the
// context contains a valid span context.
func NewTraceHandler(h slog.Handler) slog.Handler {
	return &TraceHandler{Handler: h}
}

func (h *TraceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &TraceHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *TraceHandler) WithGroup(name string) slog.Handler {
	return &TraceHandler{Handler: h.Handler.WithGroup(name)}
}

func (h *TraceHandler) Handle(ctx context.Context, r slog.Record) error {
	sc := trace.SpanContextFromContext(ctx)
	if sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
			slog.String("trace_flags", sc.TraceFlags().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}
