package platform_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/auarstadtt/platform"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestRequestID_MintsAndEchoes(t *testing.T) {
	var seen string
	h := platform.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = platform.RequestIDFrom(r.Context())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if seen == "" {
		t.Fatal("no request ID placed in the context")
	}
	if got := rec.Header().Get(platform.HeaderRequestID); got != seen {
		t.Fatalf("response header %q does not match context ID %q", got, seen)
	}
	if len(seen) != 32 {
		t.Fatalf("expected 128-bit hex ID (32 chars), got %d: %q", len(seen), seen)
	}
}

func TestRequestID_AdoptsValidInboundID(t *testing.T) {
	var seen string
	h := platform.RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = platform.RequestIDFrom(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(platform.HeaderRequestID, "upstream-abc_123.4")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if seen != "upstream-abc_123.4" {
		t.Fatalf("valid inbound ID not adopted: %q", seen)
	}
}

// A caller-supplied ID reaches both a response header and structured logs, so
// anything outside the unreserved set must be replaced rather than passed on.
func TestRequestID_RejectsUnsafeInboundIDs(t *testing.T) {
	cases := map[string]string{
		"header injection": "abc\r\nX-Evil: 1",
		"log injection":    "abc\ndef",
		"space":            "abc def",
		"too long":         strings.Repeat("a", 65),
		"empty":            "",
		"non-ascii":        "abc✓",
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			var seen string
			h := platform.RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				seen = platform.RequestIDFrom(r.Context())
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header[platform.HeaderRequestID] = []string{bad}
			h.ServeHTTP(httptest.NewRecorder(), req)

			if seen == bad {
				t.Fatalf("unsafe inbound ID was adopted: %q", bad)
			}
			if len(seen) != 32 {
				t.Fatalf("expected a freshly minted ID, got %q", seen)
			}
		})
	}
}

func TestLogger_CarriesRequestIDAndService(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	h, err := platform.Wrap(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			platform.Logger(r.Context()).Info("hello")
		}),
		platform.Options{Logger: zap.New(core), ServiceName: "testsvc"},
	)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/thing", nil))

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(entries))
	}
	fields := entries[0].ContextMap()
	if fields["request_id"] == "" || fields["request_id"] == nil {
		t.Error("log entry has no request_id — correlation is the point of this module")
	}
	if fields["request_id"] != rec.Header().Get(platform.HeaderRequestID) {
		t.Error("log request_id does not match the ID returned to the client")
	}
	if fields["service"] != "testsvc" {
		t.Errorf("service = %v, want testsvc", fields["service"])
	}
	if fields["path"] != "/thing" {
		t.Errorf("path = %v, want /thing", fields["path"])
	}
}

// Logger must never return nil, even with no middleware in play — a missing
// logger must not turn a log line into a panic.
func TestLogger_NeverNilWithoutMiddleware(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	l := platform.Logger(req.Context())
	if l == nil {
		t.Fatal("Logger returned nil")
	}
	l.Info("must not panic")
}

func TestRateLimit_BlocksOverBurstAndSets429(t *testing.T) {
	h, err := platform.Wrap(okHandler(), platform.Options{
		Logger:      zap.NewNop(),
		ServiceName: "testsvc",
		RateLimit: platform.RateLimitOptions{
			RequestsPerSecond: 1,
			Burst:             2,
			KeyFunc:           func(*http.Request) string { return "fixed" },
		},
	})
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}

	codes := make([]int, 0, 4)
	for range 4 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		codes = append(codes, rec.Code)
	}

	if codes[0] != http.StatusOK || codes[1] != http.StatusOK {
		t.Fatalf("first two requests should pass the burst, got %v", codes)
	}
	if codes[2] != http.StatusTooManyRequests || codes[3] != http.StatusTooManyRequests {
		t.Fatalf("requests beyond burst should be 429, got %v", codes)
	}
}

// A throttled health probe reads as an unhealthy pod and gets it restarted, so
// exemption has to actually work.
func TestRateLimit_ExemptPathsNeverThrottled(t *testing.T) {
	h, err := platform.Wrap(okHandler(), platform.Options{
		Logger:      zap.NewNop(),
		ServiceName: "testsvc",
		RateLimit: platform.RateLimitOptions{
			RequestsPerSecond: 1,
			Burst:             1,
			KeyFunc:           func(*http.Request) string { return "fixed" },
			Exempt:            platform.ExemptPaths("/health"),
		},
	})
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	for i := range 5 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("health probe %d throttled with %d", i, rec.Code)
		}
	}
}

func TestRateLimit_ZeroValueDisabled(t *testing.T) {
	h, err := platform.Wrap(okHandler(), platform.Options{
		Logger: zap.NewNop(), ServiceName: "testsvc",
	})
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	for range 50 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("limiting should be off by default, got %d", rec.Code)
		}
	}
}

func TestWrap_RequiresLoggerAndServiceName(t *testing.T) {
	if _, err := platform.Wrap(okHandler(), platform.Options{ServiceName: "x"}); err == nil {
		t.Error("expected an error when Logger is nil")
	}
	if _, err := platform.Wrap(okHandler(), platform.Options{Logger: zap.NewNop()}); err == nil {
		t.Error("expected an error when ServiceName is empty")
	}
}

// TestClientIP_ReadsTheChainFromTheRight pins ClientIP to the header the
// production load balancer actually delivers. The first two cases are the shape
// whoami returned behind the GKE ingress on 2026-09-30, with documentation addresses.
func TestClientIP_ReadsTheChainFromTheRight(t *testing.T) {
	cases := []struct {
		name string
		xff  []string
		want string
	}{
		{"through the load balancer", []string{"203.0.113.195,198.51.100.10, 35.191.4.170"}, "203.0.113.195"},
		{"forged left-most entry is ignored", []string{"6.6.6.6,203.0.113.195,198.51.100.10, 35.191.4.196"}, "203.0.113.195"},
		{"forged front-end address on the left is ignored", []string{"35.191.1.1,203.0.113.195,198.51.100.10, 130.211.0.9"}, "203.0.113.195"},
		{"proxy added a second header line", []string{"6.6.6.6,203.0.113.195,198.51.100.10", "35.191.4.196"}, "203.0.113.195"},
		{"not through the load balancer", []string{"203.0.113.7"}, "203.0.113.7"},
		{"not through the load balancer, right-most wins", []string{" 203.0.113.7 , 70.41.3.18 "}, "70.41.3.18"},
		{"only proxy hops falls back to the connection", []string{"198.51.100.10, 35.191.4.196"}, "10.0.0.9"},
		{"no header", nil, "10.0.0.9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = "10.0.0.9:1234"
			for _, v := range tc.xff {
				req.Header.Add("X-Forwarded-For", v)
			}
			if got := platform.ClientIP(req); got != tc.want {
				t.Fatalf("ClientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

// A Kubernetes probe fires forever; its access-log records would dominate the
// stream. The request-scoped logger must still be attached, so anything the
// handler itself logs stays correlated.
func TestAccessLog_SkipPathsSuppressRecordButKeepCorrelation(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	h, err := platform.Wrap(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" {
				platform.Logger(r.Context()).Info("probe-handled")
			}
			w.WriteHeader(http.StatusOK)
		}),
		platform.Options{
			Logger: zap.New(core), ServiceName: "testsvc",
			AccessLog: true, AccessLogSkipPaths: []string{"/health"},
		},
	)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/real", nil))

	var access, handler int
	for _, e := range logs.All() {
		switch e.Message {
		case "request":
			access++
			if e.ContextMap()["path"] == "/health" {
				t.Error("/health produced an access-log record despite being skipped")
			}
		case "probe-handled":
			handler++
			if e.ContextMap()["request_id"] == "" {
				t.Error("skipped path lost its request-scoped logger")
			}
		}
	}
	if access != 1 {
		t.Errorf("expected 1 access record (for /real), got %d", access)
	}
	if handler != 1 {
		t.Errorf("expected the handler's own log line to survive, got %d", handler)
	}
}

// A probe needs two unrelated exemptions — never rate limited, never
// access-logged — and naming it twice is how a service gets one of them wrong.
// Naming it once via HealthPath must apply both.
func TestHealthPath_AppliesBothExemptionsFromOneDeclaration(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	h, err := platform.Wrap(okHandler(), platform.Options{
		Logger: zap.New(core), ServiceName: "testsvc", AccessLog: true,
		// Deliberately no Exempt and no AccessLogSkipPaths: HealthPath alone.
		RateLimit: platform.RateLimitOptions{
			RequestsPerSecond: 1, Burst: 1,
			KeyFunc: func(*http.Request) string { return "fixed" },
		},
	})
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}

	for i := range 6 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, platform.DefaultHealthPath, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("probe %d was rate limited (%d) — a throttled probe gets the pod restarted", i, rec.Code)
		}
	}
	for _, e := range logs.All() {
		if e.Message == "request" && e.ContextMap()["path"] == platform.DefaultHealthPath {
			t.Fatal("probe produced an access-log record")
		}
	}
}

// An explicit Exempt predicate must win: a caller who has thought about
// exemption should not be silently overridden.
func TestHealthPath_ExplicitExemptWins(t *testing.T) {
	h, err := platform.Wrap(okHandler(), platform.Options{
		Logger: zap.NewNop(), ServiceName: "testsvc",
		RateLimit: platform.RateLimitOptions{
			RequestsPerSecond: 1, Burst: 1,
			KeyFunc: func(*http.Request) string { return "fixed" },
			Exempt:  func(*http.Request) bool { return false }, // exempt nothing
		},
	})
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	var last int
	for range 3 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, platform.DefaultHealthPath, nil))
		last = rec.Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("explicit Exempt was overridden by HealthPath: got %d", last)
	}
}

// "-" disables the special-casing for a service that genuinely wants its probe
// treated like any other request.
func TestHealthPath_DisabledWithDash(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	h, err := platform.Wrap(okHandler(), platform.Options{
		Logger: zap.New(core), ServiceName: "testsvc",
		AccessLog: true, HealthPath: "-",
	})
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil))
	var access int
	for _, e := range logs.All() {
		if e.Message == "request" {
			access++
		}
	}
	if access != 1 {
		t.Fatalf("HealthPath \"-\" should leave the probe logged like any request, got %d records", access)
	}
}

// --- a la carte composition ---
//
// Most services cannot use Wrap: they interleave their own middleware, and
// some rate-limit per route group with an identity-derived key, which
// only works if the limiter runs after auth. These tests exist because the
// original HealthPath shipped only on Options and was therefore unreachable for
// exactly those services — a gap no Wrap-based test could have caught.

func TestALaCarte_RequestLoggerSkipsHealthByDefault(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	// No Wrap, no AccessLogSkipPaths — the default alone must cover the probe.
	h := platform.RequestID(platform.RequestLogger(platform.LoggingOptions{
		Logger: zap.New(core), ServiceName: "svc", AccessLog: true,
	})(okHandler()))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, platform.DefaultHealthPath, nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/real", nil))

	var access int
	for _, e := range logs.All() {
		if e.Message == "request" {
			access++
			if e.ContextMap()["path"] == platform.DefaultHealthPath {
				t.Error("probe was access-logged despite the HealthPath default")
			}
		}
	}
	if access != 1 {
		t.Errorf("expected 1 access record (for /real), got %d", access)
	}
}

func TestALaCarte_RateLimitExemptsHealthByDefault(t *testing.T) {
	h := platform.RateLimit(platform.RateLimitOptions{
		RequestsPerSecond: 1, Burst: 1,
		KeyFunc: func(*http.Request) string { return "fixed" },
	})(okHandler())

	for i := range 20 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, platform.DefaultHealthPath, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("probe %d throttled (%d) without Wrap — the default did not apply", i, rec.Code)
		}
	}
}

// The per-route-group shape: the limiter runs after auth and keys on the identity that
// auth put in the context. Staff behind one office NAT share an IP, so IP keying
// would lump a whole team into one bucket.
func TestALaCarte_IdentityKeyedLimitAfterAuth(t *testing.T) {
	type userKey struct{}
	fakeAuth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(
				context.WithValue(r.Context(), userKey{}, r.Header.Get("X-User"))))
		})
	}
	limited := platform.RateLimit(platform.RateLimitOptions{
		RequestsPerSecond: 1, Burst: 2,
		KeyFunc: func(r *http.Request) string {
			if u, _ := r.Context().Value(userKey{}).(string); u != "" {
				return u
			}
			return platform.ClientIP(r)
		},
	})(okHandler())
	h := fakeAuth(limited)

	// Same IP, two different users: each must get its own bucket.
	call := func(user string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("X-User", user)
		req.Header.Set("X-Forwarded-For", "203.0.113.50") // one shared office IP
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for range 2 {
		if got := call("alice@example.com"); got != http.StatusOK {
			t.Fatalf("alice throttled inside her own burst: %d", got)
		}
	}
	if got := call("alice@example.com"); got != http.StatusTooManyRequests {
		t.Fatalf("alice past her burst should be 429, got %d", got)
	}
	if got := call("bob@example.com"); got != http.StatusOK {
		t.Fatalf("bob shares alice's IP but must have his own bucket, got %d", got)
	}
}

func TestALaCarte_HealthPathOverrideAndDisable(t *testing.T) {
	custom := platform.RateLimit(platform.RateLimitOptions{
		RequestsPerSecond: 1, Burst: 1, HealthPath: "/livez",
		KeyFunc: func(*http.Request) string { return "fixed" },
	})(okHandler())
	for range 5 {
		rec := httptest.NewRecorder()
		custom.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/livez", nil))
		if rec.Code != http.StatusOK {
			t.Fatal("custom HealthPath was not exempted")
		}
	}

	disabled := platform.RateLimit(platform.RateLimitOptions{
		RequestsPerSecond: 1, Burst: 1, HealthPath: "-",
		KeyFunc: func(*http.Request) string { return "fixed" },
	})(okHandler())
	var last int
	for range 5 {
		rec := httptest.NewRecorder()
		disabled.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, platform.DefaultHealthPath, nil))
		last = rec.Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf(`HealthPath "-" should leave the probe limited like any route, got %d`, last)
	}
}
