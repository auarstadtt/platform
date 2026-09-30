package platform_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	sentrygo "github.com/getsentry/sentry-go"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/auarstadtt/platform"
)

// testDSN is syntactically valid and points at a host that is never contacted:
// every test here replaces the transport before an event is captured.
const testDSN = "https://public@o0.ingest.sentry.io/1"

// captureTransport is a sentry-go Transport that records events in memory rather
// than shipping them, so a test can assert a panic actually reached Sentry
// instead of only that the request returned 500. It also records whether Close
// was called, which is what pins the drain in ShutdownSentry.
type captureTransport struct {
	mu     sync.Mutex
	events []*sentrygo.Event
	closed bool
	// flushOK is what Flush reports. Defaults false via newCaptureTransport to
	// mimic v0.47's unconfirmed-flush behaviour, so the shutdown test exercises
	// the path where Close is the only thing that drains.
	flushOK bool
}

func (c *captureTransport) Configure(sentrygo.ClientOptions) {}

func (c *captureTransport) SendEvent(e *sentrygo.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func (c *captureTransport) Flush(time.Duration) bool { return c.flushOK }

func (c *captureTransport) FlushWithContext(context.Context) bool { return c.flushOK }

func (c *captureTransport) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
}

func (c *captureTransport) captured() []*sentrygo.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.events
}

func (c *captureTransport) wasClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// bindCapturingHub points the global Sentry hub at an in-memory transport for the
// duration of the test, restoring the previous client afterwards.
//
// Uses the SDK directly rather than platform.InitSentry because the wrapper
// deliberately exposes no transport seam — production code must not be able to
// redirect where events go.
func bindCapturingHub(t *testing.T) *captureTransport {
	t.Helper()
	tr := &captureTransport{}
	client, err := sentrygo.NewClient(sentrygo.ClientOptions{
		Dsn:       testDSN,
		Transport: tr,
	})
	if err != nil {
		t.Fatalf("building capture client: %v", err)
	}
	bindClient(t, client)
	return tr
}

// bindClient swaps the hub's client and restores the previous one on cleanup. The
// hub is process-global, so every test touching it must clean up or it leaks into
// the next one.
func bindClient(t *testing.T, client *sentrygo.Client) {
	t.Helper()
	prev := sentrygo.CurrentHub().Client()
	sentrygo.CurrentHub().BindClient(client)
	t.Cleanup(func() { sentrygo.CurrentHub().BindClient(prev) })
}

// panicRouter is a minimal Router implementation plus a panicking handler, so
// these tests exercise UseSentry without dragging chi into platform's go.mod.
// chi.Router satisfies platform.Router structurally; this stands in for it.
type panicRouter struct {
	middlewares []func(http.Handler) http.Handler
}

func (p *panicRouter) Use(mw ...func(http.Handler) http.Handler) {
	p.middlewares = append(p.middlewares, mw...)
}

// handler composes the mounted middleware around next the way chi does: the
// first-mounted middleware ends up outermost.
func (p *panicRouter) handler(next http.Handler) http.Handler {
	h := next
	for i := len(p.middlewares) - 1; i >= 0; i-- {
		h = p.middlewares[i](h)
	}
	return h
}

func boomHandler() http.Handler {
	return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("totals engine exploded")
	})
}

func TestInitSentry_EmptyDSNIsNoOpAndLeavesHubUnconfigured(t *testing.T) {
	// Pin the hub to no client first, so this asserts InitSentry's behaviour
	// rather than whatever an earlier test left bound.
	bindClient(t, nil)

	if platform.InitSentry(platform.SentryOptions{}) {
		t.Fatal("expected false for an empty DSN")
	}
	if platform.SentryActive() {
		t.Fatal("an empty DSN must not bind a client: sentrygo.Init accepts one " +
			"and would make the hub look configured to UseSentry")
	}
}

func TestInitSentry_MalformedDSNFailsSoftAndLogs(t *testing.T) {
	bindClient(t, nil)
	core, logs := observer.New(zap.ErrorLevel)

	active := platform.InitSentry(platform.SentryOptions{
		DSN:    "not-a-dsn",
		Logger: zap.New(core),
	})

	if active {
		t.Fatal("expected false for a malformed DSN")
	}
	if platform.SentryActive() {
		t.Fatal("a failed init must not leave a client bound")
	}
	if logs.Len() != 1 {
		t.Fatalf("expected the failure to be logged once, got %d records", logs.Len())
	}
	if msg := logs.All()[0].Message; msg != "sentry init failed, continuing without error reporting" {
		t.Fatalf("unexpected log message: %q", msg)
	}
}

// TestUseSentry_ReportsPanicAndStillReturns500 is the ordering guard. Both
// effects must survive together: the panic reaches Sentry AND the client gets a
// 500. Reversing the two middlewares, or dropping Repanic, breaks exactly one of
// these while leaving the other looking fine — which is why both are asserted in
// one test.
func TestUseSentry_ReportsPanicAndStillReturns500(t *testing.T) {
	tr := bindCapturingHub(t)

	r := &panicRouter{}
	if !platform.UseSentry(r, platform.SentryHTTPOptions{}) {
		t.Fatal("expected the Sentry layer to be mounted when a client is bound")
	}

	rec := httptest.NewRecorder()
	r.handler(boomHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("Recoverer must turn the re-panic into a 500, got %d", rec.Code)
	}
	events := tr.captured()
	if len(events) != 1 {
		t.Fatalf("the panic must reach Sentry; got %d events — 0 means the "+
			"middlewares are ordered wrong", len(events))
	}
	if events[0].Level != sentrygo.LevelFatal {
		t.Fatalf("expected a fatal-level event, got %q", events[0].Level)
	}
}

// TestUseSentry_UnconfiguredStillRecovers pins the keyless path: with no client
// bound the reporting middleware is absent, but a panic must still become a 500
// rather than taking the process down. This is the local-dev and CI shape.
func TestUseSentry_UnconfiguredStillRecovers(t *testing.T) {
	bindClient(t, nil)

	r := &panicRouter{}
	if platform.UseSentry(r, platform.SentryHTTPOptions{}) {
		t.Fatal("expected false with no Sentry client bound")
	}
	if len(r.middlewares) != 1 {
		t.Fatalf("expected Recoverer alone, got %d middlewares", len(r.middlewares))
	}

	rec := httptest.NewRecorder()
	r.handler(boomHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("Recoverer alone must still handle the panic, got %d", rec.Code)
	}
}

// TestRecoverer_LogsPanicWithRequestID is why this Recoverer exists rather than
// chi's: the panic record has to carry the correlation ID, or it cannot be tied
// to the request that caused it.
func TestRecoverer_LogsPanicWithRequestID(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)

	h := platform.RequestID(platform.RequestLogger(platform.LoggingOptions{
		Logger:      zap.New(core),
		ServiceName: "example-service",
	})(platform.Recoverer(boomHandler())))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if logs.Len() != 1 {
		t.Fatalf("expected one panic record, got %d", logs.Len())
	}
	fields := logs.All()[0].ContextMap()
	if id, ok := fields["request_id"].(string); !ok || id == "" {
		t.Fatalf("panic record carries no request_id: %v", fields)
	}
	if fields["service"] != "example-service" {
		t.Fatalf("panic record carries no service tag: %v", fields)
	}
	if _, ok := fields["stack"]; !ok {
		t.Fatalf("panic record carries no stack: %v", fields)
	}
}

// TestRecoverer_RepanicsErrAbortHandler keeps net/http's documented abort signal
// working. Swallowing it would turn a deliberate connection abort into a 500 and
// leave the connection open.
func TestRecoverer_RepanicsErrAbortHandler(t *testing.T) {
	h := platform.Recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	defer func() {
		if v := recover(); v != http.ErrAbortHandler {
			t.Fatalf("expected ErrAbortHandler to propagate, recovered %v", v)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

// TestShutdownSentry_ClosesTheClientNotJustFlush is acceptance criterion 2 of
// RFLKT-5730: a flush-only implementation must fail this.
//
// The test cannot reproduce the racy production path directly — the SDK refuses
// to pair a custom Transport with the telemetry processor (client.go:425), and
// the processor is what makes Flush unreliable. So it pins the property that
// fixes it instead: Close reaches the transport. The capture transport reports an
// unconfirmed flush so the diagnostic path is covered in the same run.
func TestShutdownSentry_ClosesTheClientNotJustFlush(t *testing.T) {
	// Restores whatever was bound before this test, whatever we bind meanwhile.
	bindClient(t, nil)
	core, logs := observer.New(zap.InfoLevel)
	// InitSentry captures the logger ShutdownSentry uses for its flush diagnostic.
	if !platform.InitSentry(platform.SentryOptions{DSN: testDSN, Logger: zap.New(core)}) {
		t.Fatal("InitSentry should succeed with a valid DSN")
	}
	// Swap the real client InitSentry bound for one whose transport we can watch.
	tr := &captureTransport{}
	sentrygo.CurrentHub().BindClient(mustCaptureClient(t, tr))

	platform.ShutdownSentry(platform.DefaultSentryShutdownTimeout)

	if !tr.wasClosed() {
		t.Fatal("ShutdownSentry must Close the client: on sentry-go v0.47 Flush " +
			"returns the telemetry processor's result and never waits on the " +
			"transport, so a flush-only shutdown drops events non-deterministically")
	}
	if logs.Len() != 1 {
		t.Fatalf("an unconfirmed flush should be logged once as a diagnostic, got %d", logs.Len())
	}
}

func TestShutdownSentry_UnconfiguredIsNoOp(t *testing.T) {
	bindClient(t, nil)
	// Must not panic or block on a hub that was never configured — the keyless
	// deploy still runs this on its shutdown path.
	platform.ShutdownSentry(platform.DefaultSentryShutdownTimeout)
}

// mustCaptureClient rebuilds a client around an existing capture transport.
func mustCaptureClient(t *testing.T, tr *captureTransport) *sentrygo.Client {
	t.Helper()
	client, err := sentrygo.NewClient(sentrygo.ClientOptions{Dsn: testDSN, Transport: tr})
	if err != nil {
		t.Fatalf("building capture client: %v", err)
	}
	return client
}
