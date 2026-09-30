package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// HeaderRequestID is the request-correlation header. It is both read (so a
// caller's or the ingress's ID is preserved across a hop) and written back, so a
// client can quote the ID when reporting a problem.
const HeaderRequestID = "X-Request-Id"

// maxInboundIDLen bounds an ID we accept from a caller. Request IDs land in logs
// and in a response header; an unbounded caller-controlled value is a log-volume
// and header-size problem, so anything longer is replaced rather than truncated
// (a truncated ID would collide with the caller's real one and mislead).
const maxInboundIDLen = 64

type requestIDKey struct{}

// NewRequestID returns a fresh 128-bit hex request ID.
//
// crypto/rand rather than math/rand: request IDs appear in URLs users paste into
// tickets and in logs shared across teams, so they should not be guessable
// enough to let someone probe for another request's trace.
func NewRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice on the platforms we deploy to,
		// but a request must still get an ID — correlation degrading is better
		// than a request failing.
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}

// WithRequestID stores a request ID in the context.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFrom returns the context's request ID, or "" when none is set.
func RequestIDFrom(ctx context.Context) string {
	id, ok := ctx.Value(requestIDKey{}).(string)
	if !ok {
		return ""
	}
	return id
}

// validInboundID reports whether a caller-supplied ID is safe to adopt.
//
// Restricted to an unreserved subset rather than merely rejecting newlines: the
// value is echoed in a response header and written into structured logs, so
// keeping it to [A-Za-z0-9._-] avoids header-injection and log-injection
// entirely instead of relying on every downstream sink to escape correctly.
func validInboundID(s string) bool {
	if s == "" || len(s) > maxInboundIDLen {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// RequestID adopts a valid inbound X-Request-Id or mints a new one, puts it in
// the context, and echoes it on the response.
//
// Placed first in the chain so every later middleware — logging, Sentry, rate
// limiting — can attribute what it does to a request.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if !validInboundID(id) {
			id = NewRequestID()
		}
		w.Header().Set(HeaderRequestID, id)
		next.ServeHTTP(w, r.WithContext(WithRequestID(r.Context(), id)))
	})
}
