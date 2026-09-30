package platform

import (
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	sentrygo "github.com/getsentry/sentry-go"
	sentryhttp "github.com/getsentry/sentry-go/http"
	"go.uber.org/zap"
)

// DefaultSentryShutdownTimeout bounds the flush attempt in ShutdownSentry. Two
// seconds is the SDK's own default and comfortably inside a Kubernetes
// terminationGracePeriodSeconds, so a pod being rolled still drains.
const DefaultSentryShutdownTimeout = 2 * time.Second

// Router is the subset of a router this package needs to mount middleware:
// chi.Router satisfies it structurally, as does anything else exposing the same
// Use signature.
//
// Declared here rather than importing chi because platform is imported by every
// Go service — a router in this go.mod becomes a router in all of them,
// including the ones that serve only gRPC. The interface costs one
// declaration and keeps the dependency surface to the Sentry SDK alone.
type Router interface {
	Use(middlewares ...func(http.Handler) http.Handler)
}

// SentryOptions configures error reporting.
type SentryOptions struct {
	// DSN is the Sentry project key. Empty disables reporting entirely, which is
	// the local-dev and CI case.
	DSN string
	// Environment separates staging from production events in the Sentry UI.
	Environment string
	// Release ties an event to a deployed version so a regression can be dated.
	Release string
	// Logger reports a failed init and an unconfirmed flush. Optional.
	Logger *zap.Logger
}

// sentryLogger is captured by InitSentry and reused by ShutdownSentry to report
// an unconfirmed delivery. Package-level so ShutdownSentry keeps the signature
// the shutdown path wants — a one-liner in a defer — rather than threading a
// logger through it.
var (
	sentryMu     sync.Mutex
	sentryLogger *zap.Logger
)

// InitSentry configures the process-global Sentry hub and reports whether error
// reporting is now active. Callers do not need the return value to gate
// UseSentry, which detects the hub state itself; it is there for a boot log.
//
// Fail-soft in both directions. An empty DSN is not an error: local dev, `go
// test` and any keyless deploy must boot unchanged. A malformed DSN logs and
// returns false rather than aborting startup — taking a service down over a bad
// observability key trades a real outage for a monitoring gap.
//
// The empty-DSN short-circuit is load-bearing, not just an optimization:
// sentrygo.Init accepts an empty DSN and binds a working client that discards
// everything, which would make the hub look configured to UseSentry and get the
// reporting middleware installed for nothing.
func InitSentry(o SentryOptions) bool {
	if o.DSN == "" {
		return false
	}
	if err := sentrygo.Init(sentrygo.ClientOptions{
		Dsn:              o.DSN,
		Environment:      o.Environment,
		Release:          o.Release,
		AttachStacktrace: true,
	}); err != nil {
		if o.Logger != nil {
			o.Logger.Error("sentry init failed, continuing without error reporting", zap.Error(err))
		}
		return false
	}
	sentryMu.Lock()
	sentryLogger = o.Logger
	sentryMu.Unlock()
	return true
}

// SentryActive reports whether a Sentry client is bound to the global hub.
//
// Derived from the hub rather than from a flag InitSentry sets, so the state
// cannot drift from reality — a test that binds its own client, or a service
// that calls the SDK directly, is reported accurately.
func SentryActive() bool {
	return sentrygo.CurrentHub().Client() != nil
}

// ShutdownSentry drains buffered events, then closes the client. No-op when
// Sentry was never configured. Call once on graceful shutdown so an error
// captured moments before SIGTERM is not lost with the process.
//
// FLUSH ALONE IS NOT ENOUGH on sentry-go v0.47, and this is verified rather than
// theoretical. v0.47 routes events through a telemetry Processor; Client.Flush
// then delegates to FlushWithContext, which returns the processor's result and
// never waits on Client.Transport (client.go:750 and :778). The outcome is racy,
// not deterministically broken: during RFLKT-5726 two probe events reported
// Flush == true, logged "Buffer flushed successfully", and never reached the
// project, while an identical rerun moments later did deliver. Client.Close is
// the deterministic drain — it closes the processor with its own 5s budget and
// then the transport (client.go:788) — so it always runs after the flush
// attempt. TestShutdownSentry_ClosesTheClientNotJustFlush pins this; a
// flush-only implementation fails it.
//
// The Flush result is logged rather than trusted: it can read false on a run
// that Close then completes, so it is a diagnostic, not an alarm.
func ShutdownSentry(timeout time.Duration) {
	client := sentrygo.CurrentHub().Client()
	if client == nil {
		return
	}
	if delivered := client.Flush(timeout); !delivered {
		sentryMu.Lock()
		l := sentryLogger
		sentryMu.Unlock()
		if l != nil {
			l.Info("sentry flush did not confirm delivery; closing client to drain the processor",
				zap.Duration("timeout", timeout))
		}
	}
	client.Close()
}

// Recoverer converts a handler panic into a 500 and logs it through the
// request-scoped logger, so the record carries the request ID that correlates it
// with everything else the request did.
//
// Ships here rather than deferring to chi's Recoverer for that reason: chi's
// writes to its own logger, which is exactly the uncorrelated output this module
// exists to eliminate. Keeping it also means no router in platform's go.mod.
//
// http.ErrAbortHandler is re-panicked rather than reported: it is net/http's
// documented way for a handler to abandon a response, so it is a normal control
// flow signal and swallowing it would leave the connection open.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v)
			}
			Logger(r.Context()).Error("panic recovered",
				zap.Any("panic", v),
				zap.ByteString("stack", debug.Stack()),
			)
			w.WriteHeader(http.StatusInternalServerError)
		}()
		next.ServeHTTP(w, r)
	})
}

// UseSentry mounts the panic-reporting chain on r in the one order that works,
// and reports whether the Sentry layer was included. Recoverer is mounted either
// way, so a keyless deploy still turns a panic into a 500 instead of losing the
// pod.
//
// The order is load-bearing and both failure modes were verified by deliberately
// breaking them during RFLKT-5726:
//
//   - Recoverer first makes it the OUTER wrapper. A handler panic reaches
//     sentryhttp first, which reports it and (Repanic) re-panics; Recoverer then
//     catches that and writes the 500. Both effects happen.
//   - Mount sentryhttp outside Recoverer and 0 events are reported — Recoverer
//     eats the panic before sentryhttp ever sees it.
//   - Drop Repanic and sentryhttp eats the panic itself, returning HTTP 200 on a
//     request that panicked. A failed save then looks successful to the client.
//
// Because that is a correctness property rather than a preference, Repanic is not
// configurable.
func UseSentry(r Router, o SentryHTTPOptions) bool {
	r.Use(Recoverer)
	if !SentryActive() {
		return false
	}
	r.Use(sentryhttp.New(sentryhttp.Options{
		Repanic:         true,
		WaitForDelivery: o.WaitForDelivery,
		Timeout:         o.Timeout,
	}).Handle)
	return true
}

// SentryHTTPOptions configures the reporting middleware. The zero value is right
// for a long-lived server, which drains on shutdown instead.
type SentryHTTPOptions struct {
	// WaitForDelivery blocks the panicking request until its event is sent.
	//
	// For a short-lived process — a Job or CronJob that may exit before
	// ShutdownSentry drains — this is the difference between having the panic and
	// losing it. On a long-lived server it adds request latency to no end.
	WaitForDelivery bool
	// Timeout bounds that wait. Defaults to the SDK's 2s when zero. Only
	// consulted when WaitForDelivery is set.
	Timeout time.Duration
}
