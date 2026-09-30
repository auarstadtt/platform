package platform

import (
	"context"
	"net/http"
	"time"

	"go.uber.org/zap"
)

type loggerKey struct{}

// fallbackLogger is returned by Logger when no request-scoped logger is present.
// A no-op rather than a nil pointer, so a call site can never panic for the sake
// of a log line — and no-op rather than a real stderr logger so a missing
// middleware shows up as absent logs (an obvious symptom) instead of logs that
// silently lack correlation.
var fallbackLogger = zap.NewNop()

// WithLogger stores a logger in the context.
func WithLogger(ctx context.Context, l *zap.Logger) context.Context {
	if l == nil {
		return ctx
	}
	return context.WithValue(ctx, loggerKey{}, l)
}

// Logger returns the request-scoped logger, which already carries request_id and
// service fields. Never nil.
//
// This is the accessor that makes correlation IDs real. A logger reached as a
// package global rather than through the request context produces records that
// cannot be tied to a request. Call this instead:
//
//	platform.Logger(ctx).Info("booking created", zap.String("ref", ref))
func Logger(ctx context.Context) *zap.Logger {
	l, ok := ctx.Value(loggerKey{}).(*zap.Logger)
	if !ok || l == nil {
		return fallbackLogger
	}
	return l
}

// statusRecorder captures the status code so the access log can report it.
// WriteHeader may legitimately never be called (an empty 200), hence the default.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status, s.wrote = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.status, s.wrote = http.StatusOK, true
	}
	return s.ResponseWriter.Write(b)
}

// LoggingOptions configures RequestLogger.
type LoggingOptions struct {
	// Logger is the base logger. Required.
	Logger *zap.Logger
	// ServiceName tags every record, so one log stream can be filtered per
	// service when several ship to the same sink.
	ServiceName string
	// AccessLog emits one record per completed request. Off by default: several
	// services already sit behind an ingress that logs access, and duplicating
	// it doubles log spend for no new information.
	AccessLog bool

	// AccessLogSkipPaths suppresses the access-log record for these exact paths,
	// while still attaching the request-scoped logger so anything the handler
	// logs itself is still correlated.
	//
	// You do not need to list the health probe here — see HealthPath.
	AccessLogSkipPaths []string

	// HealthPath is the Kubernetes probe endpoint, skipped in the access log
	// automatically. Defaults to DefaultHealthPath; set "-" to disable.
	//
	// /health fires every few seconds forever, and in one upstream service it
	// drowned the logs badly enough to warrant its own ticket (RFLKT-5203).
	// Every service behind the cluster has this problem, so the default handles
	// it rather than each repo remembering.
	//
	// Present on this struct, not only on Options, because most services compose
	// these middlewares individually instead of via Wrap — they interleave their
	// own (Sentry, CORS, auth) and cannot hand the whole chain over.
	HealthPath string
}

// RequestLogger derives a per-request logger carrying request_id (plus method
// and path) and puts it in the context for Logger(ctx) to find.
//
// Must run after RequestID, or request_id is empty.
func RequestLogger(o LoggingOptions) func(http.Handler) http.Handler {
	base := o.Logger
	if base == nil {
		base = fallbackLogger
	}
	if o.ServiceName != "" {
		base = base.With(zap.String("service", o.ServiceName))
	}
	skip := make(map[string]struct{}, len(o.AccessLogSkipPaths)+1)
	for _, p := range o.AccessLogSkipPaths {
		skip[p] = struct{}{}
	}
	if hp := ResolveHealthPath(o.HealthPath); hp != "" {
		skip[hp] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			l := base.With(
				zap.String("request_id", RequestIDFrom(r.Context())),
				zap.String("method", r.Method),
				zap.String("path", r.URL.Path),
			)
			ctx := WithLogger(r.Context(), l)

			_, skipped := skip[r.URL.Path]
			if !o.AccessLog || skipped {
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			start := time.Now()
			next.ServeHTTP(rec, r.WithContext(ctx))
			l.Info("request",
				zap.Int("status", rec.status),
				zap.Duration("duration", time.Since(start)),
			)
		})
	}
}
