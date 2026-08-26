package obs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
	"strings"
)

type ctxKey int

const requestIDKey ctxKey = iota

// RequestIDHeader is the header a request id travels in, both inbound from a
// client and outbound to a downstream service.
const RequestIDHeader = "X-Request-Id"

// WithRequestID attaches a request id to a context.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFrom returns the request id carried by a context, or "".
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// NewRequestID returns a short random identifier.
//
// Short on purpose: it appears on every log line of every service a request
// touches, and a full UUID would triple the width of that column without
// making collisions meaningfully less likely at this scale.
func NewRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}

// ContextHandler decorates every record with the request-scoped attributes
// held in its context.
//
// This is what makes a single customer's checkout traceable across five
// services: the id is attached once at the edge, propagated over HTTP, and
// appears on every line without any call site having to remember to log it.
// Call sites only have to use the ...Context logging variants.
type ContextHandler struct {
	slog.Handler
}

// Handle adds the request id, when there is one, and delegates.
func (h ContextHandler) Handle(ctx context.Context, record slog.Record) error {
	if id := RequestIDFrom(ctx); id != "" {
		record.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, record)
}

// WithAttrs preserves the wrapper when attributes are added.
func (h ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return ContextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

// WithGroup preserves the wrapper when a group is opened.
func (h ContextHandler) WithGroup(name string) slog.Handler {
	return ContextHandler{Handler: h.Handler.WithGroup(name)}
}

// InitLogging installs the process-wide logger.
//
// The default format is JSON, because in production these lines are parsed by
// a collector rather than read by a person. LOG_FORMAT=text switches to the
// human-readable form, which is what the local compose stack uses.
func InitLogging(service string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(os.Getenv("LOG_LEVEL"))}

	var base slog.Handler
	if strings.EqualFold(os.Getenv("LOG_FORMAT"), "text") {
		base = slog.NewTextHandler(os.Stdout, opts)
	} else {
		base = slog.NewJSONHandler(os.Stdout, opts)
	}

	build := Build()
	logger := slog.New(ContextHandler{Handler: base}).With(
		slog.String("service", service),
		slog.String("version", build.Version),
	)
	slog.SetDefault(logger)
	return logger
}

func parseLevel(raw string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
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
