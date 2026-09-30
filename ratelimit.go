package platform

import (
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/time/rate"
)

// Limiter decides whether a key may proceed. Implemented in-process by
// NewMemoryLimiter; a Redis-backed implementation can satisfy the same interface
// so limits hold across replicas rather than per pod.
//
// An interface rather than a hard Redis dependency because not every service has
// Redis. Requiring it would mean either adding Redis to services that do not need
// it, or shipping no rate limiting at all in them — and a per-pod limit is a large
// improvement over none.
type Limiter interface {
	// Allow reports whether the key may proceed now.
	Allow(key string) bool
}

// RateLimitOptions configures the rate-limit middleware. Zero value disables
// limiting, so a service opts in explicitly rather than inheriting a limit that
// silently throttles it.
type RateLimitOptions struct {
	// RequestsPerSecond is the sustained per-key rate. 0 disables limiting.
	RequestsPerSecond float64
	// Burst is the bucket depth. Defaults to ceil(RequestsPerSecond) when 0, so
	// a misconfigured burst cannot pin the effective rate to zero.
	Burst int
	// Limiter overrides the default in-process limiter (e.g. a Redis-backed one
	// so the limit holds across replicas).
	Limiter Limiter
	// KeyFunc derives the bucket key. Defaults to the client IP.
	//
	// Prefer an identity-derived key for authenticated routes. Staff behind one
	// office NAT share a single IP, so IP keying lumps a whole team into one
	// bucket. Reading the user from the request context requires this middleware
	// to run AFTER auth, which is why it is usable standalone and per route
	// group rather than only through Wrap:
	//
	//	KeyFunc: func(r *http.Request) string {
	//	    if u := auth.FromContext(r.Context()); u != nil { return u.Email }
	//	    return ClientIP(r)
	//	}
	KeyFunc func(*http.Request) string

	// Exempt reports whether a request bypasses limiting entirely. The health
	// probe is handled by HealthPath; use this for anything else.
	//
	// An explicitly-set Exempt takes over completely — HealthPath is not folded
	// in, so a caller who has thought about exemption is never second-guessed.
	// If you set this and still want the probe exempt, include it yourself.
	Exempt func(*http.Request) bool

	// HealthPath is the Kubernetes probe endpoint, exempt from limiting
	// automatically. Defaults to DefaultHealthPath; set "-" to disable.
	//
	// A throttled readiness probe reads as an unhealthy pod and gets it
	// restarted, so this must be right by default rather than by remembering.
	HealthPath string
}

// memoryLimiter is a per-key token bucket held in process.
//
// Bounded by maxKeys and cleared wholesale when exceeded. That is deliberately
// crude: the keyspace is attacker-controlled (client IPs), so an unbounded map is
// a memory-exhaustion vector — the limiter itself becomes the outage. Dropping
// every bucket briefly lets through more traffic than intended, which is a far
// better failure than the process dying.
type memoryLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*rate.Limiter
	limit    rate.Limit
	burst    int
	maxKeys  int
	lastPrun time.Time
}

// NewMemoryLimiter returns an in-process per-key token bucket. Limits apply per
// pod, so N replicas admit up to N times the rate — acceptable for abuse
// protection, not for a contractual quota. Use a shared Limiter for that.
func NewMemoryLimiter(rps float64, burst int) Limiter {
	if burst <= 0 {
		burst = int(rps) + 1
	}
	return &memoryLimiter{
		buckets: make(map[string]*rate.Limiter),
		limit:   rate.Limit(rps),
		burst:   burst,
		maxKeys: 50_000,
	}
}

func (m *memoryLimiter) Allow(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.buckets) > m.maxKeys && time.Since(m.lastPrun) > time.Minute {
		m.buckets = make(map[string]*rate.Limiter)
		m.lastPrun = time.Now()
	}
	b, ok := m.buckets[key]
	if !ok {
		b = rate.NewLimiter(m.limit, m.burst)
		m.buckets[key] = b
	}
	return b.Allow()
}

// ClientIP extracts the client address from the X-Forwarded-For chain as the
// Digimidi GKE platform builds it, reading from the RIGHT.
//
// The inherited rule took the left-most entry, which is only safe behind a proxy
// that overwrites the header. This platform has none. The Google global external
// Application Load Balancer APPENDS "<client>,<load balancer>" to whatever the
// client sent, and Traefik, which trusts Google's ranges, appends the front end
// it received the connection from. The shape observed on production (2026-09-30,
// addresses replaced with documentation ranges) with a forged header:
//
//	X-Forwarded-For: 6.6.6.6,203.0.113.195,198.51.100.10, 35.191.4.196
//	                 forged  client        load balancer  front end
//
// The left-most rule returns 6.6.6.6, so every client picks its own rate-limit
// bucket. Only the right-hand entries are written by our infrastructure, so
// they are the only ones read: drop the front-end hops, drop the load
// balancer's address that Google always writes immediately after the client,
// and what is left on the right is the client.
//
// A chain that does not end in a front-end hop did not come through the load
// balancer (the tailnet, a port-forward). Its right-most entry is the peer the
// last proxy saw, which is not client-controlled either. With no header at all
// the connection's own address is used.
func ClientIP(r *http.Request) string {
	hops := forwardedHops(r.Header.Values("X-Forwarded-For"))
	i := len(hops) - 1
	viaFrontEnd := false
	for i >= 0 && isGoogleFrontEnd(hops[i]) {
		viaFrontEnd = true
		i--
	}
	if viaFrontEnd {
		i-- // the load balancer's own address
	}
	if i >= 0 {
		return hops[i]
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// googleFrontEnds are the source ranges of Google's L7 load balancer proxies,
// the same list modules/traefik in digimidi-cloud-infrastructure trusts through
// forwarded_headers_trusted_ips. The two must change together: a range Traefik
// trusts but this list lacks makes ClientIP return the load balancer's address,
// and every client shares one bucket.
var googleFrontEnds = []netip.Prefix{
	netip.MustParsePrefix("35.191.0.0/16"),
	netip.MustParsePrefix("130.211.0.0/22"),
}

func isGoogleFrontEnd(hop string) bool {
	addr, err := netip.ParseAddr(hop)
	if err != nil {
		return false
	}
	for _, p := range googleFrontEnds {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// forwardedHops flattens every X-Forwarded-For line into one ordered list.
// A proxy may add a second header line instead of extending the first, and
// reading only the first would drop exactly the entries our proxies wrote.
func forwardedHops(lines []string) []string {
	var hops []string
	for _, line := range lines {
		for _, hop := range strings.Split(line, ",") {
			if hop = strings.TrimSpace(hop); hop != "" {
				hops = append(hops, hop)
			}
		}
	}
	return hops
}

// RateLimit rejects requests exceeding the configured per-key rate with 429 and a
// Retry-After header. A zero RequestsPerSecond returns a pass-through.
func RateLimit(o RateLimitOptions) func(http.Handler) http.Handler {
	if o.RequestsPerSecond <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	lim := o.Limiter
	if lim == nil {
		lim = NewMemoryLimiter(o.RequestsPerSecond, o.Burst)
	}
	keyFn := o.KeyFunc
	if keyFn == nil {
		keyFn = ClientIP
	}
	// An explicit Exempt wins outright; otherwise the health probe is exempt by
	// default. Not merged, so a caller who set Exempt gets exactly what they
	// asked for rather than that plus a surprise.
	exempt := o.Exempt
	if exempt == nil {
		if hp := ResolveHealthPath(o.HealthPath); hp != "" {
			exempt = ExemptPaths(hp)
		}
	}
	retryAfter := strconv.Itoa(max(1, int(1/o.RequestsPerSecond)))

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if exempt != nil && exempt(r) {
				next.ServeHTTP(w, r)
				return
			}
			if !lim.Allow(keyFn(r)) {
				// Logged at Warn with the request ID attached, so a burst of
				// 429s is attributable rather than an unexplained traffic dip.
				Logger(r.Context()).Warn("rate limited", zap.String("key", keyFn(r)))
				w.Header().Set("Retry-After", retryAfter)
				http.Error(w, "too many requests", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
