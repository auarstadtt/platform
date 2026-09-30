package platform

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// State is what the provider could tell us about a flag, as distinct from what
// the flag says. The three cases are deliberately separate because two of them
// were previously both spelled "false", which is what made retiring a flag
// dangerous (RFLKT-6033).
//
// An upstream service's flag read its default via a single fallback argument
// that applied only when evaluation was unconfigured. Production configures
// evaluation, so a flag the dashboard no longer defined resolved false there.
// Deleting the flag in PostHog before deleting the code that read it therefore
// flipped production onto the off branch — in that incident, onto a page the
// same PR had removed. The caller had no way to say "undefined means
// on now"; there was one knob for two situations.
type State int

const (
	// StateUnconfigured means flag evaluation is not running at all: no
	// provider client, no API key, log-only mode. Staging, local dev and tests
	// live here, which is why in-development surfaces default visible.
	StateUnconfigured State = iota

	// StateUndefined means evaluation is running but produced no verdict for
	// this key: usually the provider has no definition for it, which is what
	// production looks like after a dashboard delete, but a transient provider
	// error belongs here too.
	//
	// Both map here on purpose. A binding must NOT report an error as
	// StateUnconfigured, because in-development surfaces typically set
	// WhenUnconfigured true, so a provider blip would reveal them in
	// production. WhenUndefined is the fail-closed default for exactly this.
	StateUndefined

	// StateResolved means the provider returned a verdict for this key and
	// this distinct ID. The verdict wins; no default applies.
	StateResolved
)

// String renders a State for error messages and drift reports.
func (s State) String() string {
	switch s {
	case StateUnconfigured:
		return "unconfigured"
	case StateUndefined:
		return "undefined"
	case StateResolved:
		return "resolved"
	default:
		return fmt.Sprintf("State(%d)", int(s))
	}
}

// Evaluator is the provider binding. It is an interface, not a PostHog client,
// because every Go service imports this module: adding posthog-go to go.mod
// here would land it in all of them, which the dependency-surface rule in
// CLAUDE.md forbids. The service implements this over whatever SDK it already
// has, and platform stays at zap, x/time, grpc and sentry-go.
//
// Implementations must not block for long: Enabled is called on request paths.
// A PostHog binding with local evaluation against a polled flag definition set
// does no I/O per call, which is the shape to aim for.
type Evaluator interface {
	// Evaluate reports the flag's value for distinctID plus how much the
	// provider actually knew.
	//
	// Reserve StateUnconfigured for the structural case: no client, no API
	// key, log-only mode. A provider that is configured but fails to answer —
	// an error, a definition it does not have — is StateUndefined, which is
	// the fail-closed side. Reporting a transient error as unconfigured would
	// hand it the WhenUnconfigured default, which for an in-development
	// surface is usually "visible".
	//
	// Targeting context (tenant, person properties) is derived from ctx by the
	// implementation: it is provider-specific and platform has no vocabulary
	// for it.
	Evaluate(ctx context.Context, key, distinctID string) (value bool, state State)
}

// Kind is why a flag exists, which determines whether it is allowed to outlive
// its expiry date.
type Kind string

const (
	// KindRollout ships one change progressively and is meant to die. It
	// requires an Expires date, and Expired reports it once past.
	KindRollout Kind = "rollout"

	// KindKillSwitch disables a shipped feature in an incident. Expires is
	// optional: a kill switch legitimately sits at "on" for a long time, but
	// setting a date still gets it reviewed.
	KindKillSwitch Kind = "killswitch"

	// KindPermanent is long-lived configuration expressed as a flag, e.g. a
	// per-tenant capability. Never reported as expired.
	KindPermanent Kind = "permanent"
)

// Spec declares one flag and, crucially, what it means in each State. Both
// defaults are explicit because the two situations genuinely differ and the
// difference is not guessable from the call site.
type Spec struct {
	// Key is the provider's flag key, e.g. "new-checkout".
	Key string

	// Kind drives expiry enforcement. Required.
	Kind Kind

	// Owner is who to ask before deleting it. Required for rollout and kill
	// switches: an expired flag with no owner is how a flag survives a year.
	Owner string

	// Ticket is the Linear ID that introduced the flag. Required for rollout
	// and kill switches, so the expiry failure points at the original context.
	Ticket string

	// Expires is when this flag should be gone. Required for KindRollout.
	// Compared against a caller-supplied clock, never time.Now, so the expiry
	// test is deterministic.
	Expires time.Time

	// WhenUnconfigured is the value where evaluation is not running: staging,
	// local dev, tests. Usually true for an in-development UI surface, so the
	// team sees it without a dashboard round trip.
	WhenUnconfigured bool

	// WhenUndefined is the value where evaluation IS running but the provider
	// has no definition. Usually false while a flag is rolling out: an
	// unlaunched feature should stay dark if someone fat-fingers the dashboard.
	//
	// Flip it to true to GRADUATE a flag that has reached 100 percent. That one
	// line is what makes retirement safe: after the deploy carrying it, the
	// provider definition and the code can be deleted in either order, because
	// "the dashboard no longer defines this" now resolves to the same value the
	// dashboard was returning. Without it, the deletion order is load-bearing
	// and getting it wrong is a production regression (RFLKT-6033).
	WhenUndefined bool
}

// Graduated reports whether the spec is safe to retire in any order — that is,
// whether an absent provider definition resolves the same way a fully rolled
// out one does.
func (s Spec) Graduated() bool { return s.WhenUndefined }

// Registry is the set of flags a service declares, plus its provider binding.
// It is the single place a flag's existence is written down, which is what
// makes the drift check and the expiry test possible at all.
type Registry struct {
	eval  Evaluator
	specs map[string]Spec
	keys  []string
}

// Registry construction errors. Returned rather than panicked so a
// misconfiguration surfaces in the caller's normal startup error path, matching
// Wrap.
var (
	ErrNoFlagKey       = errors.New("platform: Spec.Key is required")
	ErrDuplicateFlag   = errors.New("platform: duplicate Spec.Key")
	ErrUnknownKind     = errors.New("platform: unknown Spec.Kind")
	ErrNoExpiry        = errors.New("platform: KindRollout requires Spec.Expires")
	ErrNoOwnerOrTicket = errors.New("platform: Spec.Owner and Spec.Ticket are required for rollout and kill switches")
)

// NewRegistry validates and freezes a flag set. A nil Evaluator is valid and
// resolves every flag as StateUnconfigured, which is what tests and keyless
// local runs want.
func NewRegistry(eval Evaluator, specs ...Spec) (*Registry, error) {
	r := &Registry{eval: eval, specs: make(map[string]Spec, len(specs))}
	for _, s := range specs {
		if s.Key == "" {
			return nil, ErrNoFlagKey
		}
		if _, dup := r.specs[s.Key]; dup {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateFlag, s.Key)
		}
		switch s.Kind {
		case KindRollout, KindKillSwitch:
			if s.Owner == "" || s.Ticket == "" {
				return nil, fmt.Errorf("%w: %s", ErrNoOwnerOrTicket, s.Key)
			}
			if s.Kind == KindRollout && s.Expires.IsZero() {
				return nil, fmt.Errorf("%w: %s", ErrNoExpiry, s.Key)
			}
		case KindPermanent:
		default:
			return nil, fmt.Errorf("%w: %q on %s", ErrUnknownKind, s.Kind, s.Key)
		}
		r.specs[s.Key] = s
		r.keys = append(r.keys, s.Key)
	}
	sort.Strings(r.keys)
	return r, nil
}

// Enabled resolves a flag for distinctID. An unregistered key returns false:
// the registry is the declaration, so a key that is not in it is a bug, and
// failing closed keeps an undeclared gate dark rather than open.
func (r *Registry) Enabled(ctx context.Context, key, distinctID string) bool {
	spec, ok := r.specs[key]
	if !ok {
		return false
	}
	var (
		value bool
		state = StateUnconfigured
	)
	if r.eval != nil {
		value, state = r.eval.Evaluate(ctx, key, distinctID)
	}
	switch state {
	case StateResolved:
		return value
	case StateUndefined:
		return spec.WhenUndefined
	default:
		// Overrides apply only here, so the developer switch can never touch a
		// deployment where the provider is authoritative.
		if o, has := overrideFromContext(ctx, key); has {
			return o
		}
		return spec.WhenUnconfigured
	}
}

// Lookup returns a declared spec.
func (r *Registry) Lookup(key string) (Spec, bool) {
	s, ok := r.specs[key]
	return s, ok
}

// Keys returns every declared key, sorted. Feeds the drift check that compares
// what the code reads against what the provider defines.
func (r *Registry) Keys() []string {
	out := make([]string, len(r.keys))
	copy(out, r.keys)
	return out
}

// Expired returns the specs that should already have been deleted, sorted by
// key. Rollouts and dated kill switches expire; KindPermanent never does.
func (r *Registry) Expired(now time.Time) []Spec {
	var out []Spec
	for _, k := range r.keys {
		s := r.specs[k]
		if s.Kind == KindPermanent || s.Expires.IsZero() {
			continue
		}
		if now.After(s.Expires) {
			out = append(out, s)
		}
	}
	return out
}

// TB is the subset of testing.TB that AssertNoExpired needs. Declared locally
// so this file does not import testing into every service's production binary.
type TB interface {
	Helper()
	Errorf(format string, args ...any)
}

// AssertNoExpired fails the test for every flag past its date. A service wires
// this into one test and the build then turns red on the day a rollout flag was
// supposed to be gone, instead of the flag quietly living forever — which is
// how one upstream flag sat at 100 percent with no conditions until someone
// happened to look (RFLKT-6033).
//
// Takes now explicitly so the test is deterministic and can be exercised for
// both the expired and the not-yet-expired case.
func (r *Registry) AssertNoExpired(t TB, now time.Time) {
	t.Helper()
	for _, s := range r.Expired(now) {
		t.Errorf("feature flag %q (%s, owner %s, %s) expired %s: delete it or move its date",
			s.Key, s.Kind, s.Owner, s.Ticket, s.Expires.Format(time.DateOnly))
	}
}

// overrideCookie carries every developer override in one cookie, rather than
// one cookie per flag. The per-flag alternative is what made RFLKT-6033
// expensive: one flag grew a dedicated cookie, a query param, a
// sidebar switch, its CSS and its JS, all of which had to be deleted by hand.
const overrideCookie = "ff_overrides"

// overrideMaxAge keeps a developer's choice for a working month.
const overrideMaxAge = 60 * 60 * 24 * 30

// overrideParamPrefix is the query-string form: ?ff_new-checkout=on.
const overrideParamPrefix = "ff_"

type overrideCtxKey struct{}

// Override reads ?ff_<key>=on|off, merges it into the ff_overrides cookie, and
// puts the resulting set on the request context. It is a no-op for any flag the
// provider can actually resolve, because Enabled consults overrides only in
// StateUnconfigured — so this is inert in production by construction rather
// than by a separate environment check that could drift.
//
// Only declared keys are accepted. The cookie is client-supplied, so without
// that filter it would be an unbounded attacker-controlled map.
func (r *Registry) Override(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		set := r.parseOverrideCookie(req)

		changed := false
		for param, vals := range req.URL.Query() {
			key, ok := strings.CutPrefix(param, overrideParamPrefix)
			if !ok || len(vals) == 0 {
				continue
			}
			if _, declared := r.specs[key]; !declared {
				continue
			}
			switch vals[0] {
			case "on":
				set[key], changed = true, true
			case "off":
				set[key], changed = false, true
			case "clear":
				delete(set, key)
				changed = true
			}
		}

		if changed {
			http.SetCookie(w, &http.Cookie{
				Name:  overrideCookie,
				Value: encodeOverrides(set),
				Path:  "/",
				// Deleting the last override should clear the cookie, not leave
				// an empty one that looks like state.
				MaxAge:   overrideMaxAgeFor(set),
				HttpOnly: true,
				// Unconditionally Secure, which costs nothing here and keeps
				// gosec G124 satisfied without a //nolint (those need security
				// sign-off in the org pipeline). It does not break local dev:
				// overrides only apply in StateUnconfigured, which is staging
				// (HTTPS behind Traefik) and localhost, and browsers have
				// treated http://localhost as a secure context — accepting
				// Secure cookies on it — since Chrome 89 and the equivalent
				// Firefox release. The only case this excludes is plain HTTP on
				// a non-localhost host, which is not a deployment we have.
				Secure:   true,
				SameSite: http.SameSiteLaxMode,
			})
		}

		if len(set) > 0 {
			req = req.WithContext(context.WithValue(req.Context(), overrideCtxKey{}, set))
		}
		next.ServeHTTP(w, req)
	})
}

// overrideMaxAgeFor expires the cookie when the last override is cleared.
func overrideMaxAgeFor(set map[string]bool) int {
	if len(set) == 0 {
		return -1
	}
	return overrideMaxAge
}

// parseOverrideCookie decodes "key=on,other=off", dropping anything not
// declared in this registry.
func (r *Registry) parseOverrideCookie(req *http.Request) map[string]bool {
	set := make(map[string]bool)
	c, err := req.Cookie(overrideCookie)
	if err != nil {
		return set
	}
	for _, pair := range strings.Split(c.Value, ",") {
		key, val, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		if _, declared := r.specs[key]; !declared {
			continue
		}
		switch val {
		case "on":
			set[key] = true
		case "off":
			set[key] = false
		}
	}
	return set
}

// encodeOverrides renders the set deterministically so the cookie does not
// churn between requests for no reason.
func encodeOverrides(set map[string]bool) string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		if set[k] {
			b.WriteString("=on")
		} else {
			b.WriteString("=off")
		}
	}
	return b.String()
}

// overrideFromContext reads one override placed by Override. Results are named
// because two bare bools at a call site are ambiguous: the first is the flag's
// forced value, the second whether an override exists at all.
func overrideFromContext(ctx context.Context, key string) (value, has bool) {
	set, ok := ctx.Value(overrideCtxKey{}).(map[string]bool)
	if !ok {
		return false, false
	}
	value, has = set[key]
	return value, has
}
