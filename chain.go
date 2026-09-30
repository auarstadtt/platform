// Package platform provides the standard cross-cutting middleware for Digimidi
// Go services: request correlation, request-scoped logging, and rate limiting —
// wired identically everywhere via a single Wrap call — plus error reporting
// (InitSentry / UseSentry) and a feature-flag registry (NewRegistry), which are
// composed separately because they are not request middleware.
//
// It deliberately does NOT own routing, authentication, authorization, CSRF, or
// tenant scoping. Those are service-specific policy and belong in the service.
// This module owns only the concerns that have to be identical everywhere to be
// worth anything.
package platform

import (
	"errors"
	"net/http"

	"go.uber.org/zap"
)

// Options configures the standard chain.
type Options struct {
	// Logger is the service's base zap logger. Required — a service that cannot
	// log its own requests is not observable, so this is an error rather than a
	// silent fallback.
	Logger *zap.Logger

	// ServiceName tags logs. Required, for the same reason: an untagged stream
	// is ambiguous the moment two services share a sink.
	ServiceName string

	// AccessLog emits one record per completed request. Off by default (the
	// ingress already logs access for most services).
	AccessLog bool

	// AccessLogSkipPaths suppresses the access-log record for these exact paths
	// while still correlating anything the handler logs. Use it for Kubernetes
	// probes, which fire forever and otherwise dominate the log volume.
	//
	// You normally do not need this for the health probe — see HealthPath.
	AccessLogSkipPaths []string

	// HealthPath is the Kubernetes probe endpoint. Defaults to DefaultHealthPath;
	// set it to "-" to disable the special-casing entirely.
	//
	// A probe needs two unrelated exemptions and they are easy to get half-right:
	// it must not be rate limited (a throttled probe reads as an unhealthy pod and
	// gets it restarted) and it must not be access-logged (it fires every few
	// seconds forever). Requiring each service to remember both, in two different
	// option fields, is a footgun — the first pilot service had to name "/health" twice.
	// Naming it once here applies both.
	HealthPath string

	// RateLimit configures per-key limiting. Zero value disables it.
	RateLimit RateLimitOptions
}

// DefaultHealthPath is the standard probe endpoint, used when a
// HealthPath field is left empty.
const DefaultHealthPath = "/health"

// DisableHealthPath opts a service out of the probe special-casing.
const DisableHealthPath = "-"

// ResolveHealthPath is shared by every option struct that carries a HealthPath:
// the configured value, the default when empty, or "" when disabled with "-".
//
// Deliberately package-level rather than a method on Options, because the same
// defaulting has to apply to LoggingOptions and RateLimitOptions used directly.
// Most services cannot use Wrap — they interleave their own middleware, and some
// rate-limit per route group with an identity-derived key, which
// only works if the limiter runs after auth. Putting the ergonomics only on the
// aggregate would leave those services without them.
func ResolveHealthPath(configured string) string {
	switch configured {
	case "":
		return DefaultHealthPath
	case DisableHealthPath:
		return ""
	default:
		return configured
	}
}

// ErrNoLogger is returned by Wrap when Options.Logger is nil.
var ErrNoLogger = errors.New("platform: Options.Logger is required")

// ErrNoServiceName is returned by Wrap when Options.ServiceName is empty.
var ErrNoServiceName = errors.New("platform: Options.ServiceName is required")

// Wrap composes the standard chain around next, outermost first:
//
//	RequestID  -> adopt or mint the correlation ID
//	Logger     -> derive a request-scoped logger carrying that ID
//	RateLimit  -> reject abuse, logging rejections with the ID attached
//
// The order matters and is not configurable: RequestID must precede the logger
// so records carry the ID, and the logger must precede rate limiting so a
// rejection is attributable.
//
// Returns an error rather than panicking, so a misconfiguration surfaces at
// startup in the caller's normal error path.
func Wrap(next http.Handler, o Options) (http.Handler, error) {
	if o.Logger == nil {
		return nil, ErrNoLogger
	}
	if o.ServiceName == "" {
		return nil, ErrNoServiceName
	}

	// Forward the health path down rather than resolving it here, so a service
	// composing these middlewares individually gets identical behavior.
	rl := o.RateLimit
	rl.HealthPath = o.HealthPath

	h := next
	h = RateLimit(rl)(h)
	h = RequestLogger(LoggingOptions{
		Logger:             o.Logger,
		ServiceName:        o.ServiceName,
		AccessLog:          o.AccessLog,
		AccessLogSkipPaths: o.AccessLogSkipPaths,
		HealthPath:         o.HealthPath,
	})(h)
	h = RequestID(h)
	return h, nil
}

// ExemptPaths returns an Exempt predicate matching an exact set of paths. Use it
// to keep health and readiness probes out of rate limiting — a throttled probe
// reads as an unhealthy pod and gets it restarted.
func ExemptPaths(paths ...string) func(*http.Request) bool {
	set := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		set[p] = struct{}{}
	}
	return func(r *http.Request) bool {
		_, ok := set[r.URL.Path]
		return ok
	}
}
