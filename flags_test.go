package platform

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubEvaluator returns a fixed verdict, standing in for a service's PostHog
// binding. A nil entry in states means "not registered", which the tests use to
// model a provider that has no definition for the key.
type stubEvaluator struct {
	value bool
	state State
	calls int
}

func (s *stubEvaluator) Evaluate(_ context.Context, _, _ string) (bool, State) {
	s.calls++
	return s.value, s.state
}

func rolloutSpec(key string, expires time.Time) Spec {
	return Spec{
		Key: key, Kind: KindRollout, Owner: "platform-team", Ticket: "RFLKT-6035",
		Expires: expires, WhenUnconfigured: true,
	}
}

// TestEnabled_ResolvesPerState is the core of RFLKT-6035: the three provider
// states resolve independently, so a spec can say "visible in staging" and
// "dark if the dashboard loses the definition" at the same time. Before this,
// one fallback argument had to serve both and could not.
func TestEnabled_ResolvesPerState(t *testing.T) {
	spec := Spec{
		Key: "surface", Kind: KindRollout, Owner: "platform-team", Ticket: "RFLKT-6035",
		Expires:          time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
		WhenUnconfigured: true,  // staging shows in-development surfaces
		WhenUndefined:    false, // not graduated yet: stay dark
	}

	for _, tc := range []struct {
		name  string
		state State
		value bool
		want  bool
	}{
		{"resolved on beats both defaults", StateResolved, true, true},
		{"resolved off beats WhenUnconfigured", StateResolved, false, false},
		{"unconfigured uses WhenUnconfigured", StateUnconfigured, false, true},
		{"undefined uses WhenUndefined", StateUndefined, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewRegistry(&stubEvaluator{value: tc.value, state: tc.state}, spec)
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}
			if got := r.Enabled(context.Background(), "surface", "user:1"); got != tc.want {
				t.Errorf("Enabled = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEnabled_GraduatedFlagSurvivesDashboardDelete pins the property the whole
// design exists for. A graduated flag reads the same whether the provider still
// defines it or not, so the code deletion and the dashboard deletion can happen
// in either order. Under the old single-fallback model the undefined case
// resolved false in production, which is what would have served a deleted page
// in RFLKT-6033.
func TestEnabled_GraduatedFlagSurvivesDashboardDelete(t *testing.T) {
	spec := rolloutSpec("graduated", time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	spec.WhenUndefined = true // the graduation line

	if !spec.Graduated() {
		t.Fatal("Graduated() must report the spec as safe to retire in any order")
	}

	// Provider still defines it, fully rolled out.
	rolled, err := NewRegistry(&stubEvaluator{value: true, state: StateResolved}, spec)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	// Provider definition deleted; evaluation still configured (production).
	deleted, err := NewRegistry(&stubEvaluator{state: StateUndefined}, spec)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	before := rolled.Enabled(context.Background(), "graduated", "user:1")
	after := deleted.Enabled(context.Background(), "graduated", "user:1")
	if !before || !after {
		t.Errorf("graduated flag flipped on dashboard delete: before=%v after=%v", before, after)
	}
}

// TestEnabled_UngraduatedFlagWouldFlip is the negative control for the test
// above: without the graduation line, deleting the provider definition really
// does change the answer. If this ever stops failing to match, the property
// being guarded has evaporated.
func TestEnabled_UngraduatedFlagWouldFlip(t *testing.T) {
	spec := rolloutSpec("ungraduated", time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))

	rolled, _ := NewRegistry(&stubEvaluator{value: true, state: StateResolved}, spec)
	deleted, _ := NewRegistry(&stubEvaluator{state: StateUndefined}, spec)

	if rolled.Enabled(context.Background(), "ungraduated", "u") ==
		deleted.Enabled(context.Background(), "ungraduated", "u") {
		t.Error("an ungraduated flag must change value when the definition disappears; " +
			"if it does not, graduation is no longer meaningful")
	}
}

// TestEnabled_NilEvaluatorIsUnconfigured covers tests and keyless local runs:
// no provider at all behaves exactly like a provider that is not configured.
func TestEnabled_NilEvaluatorIsUnconfigured(t *testing.T) {
	r, err := NewRegistry(nil, rolloutSpec("surface", time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if !r.Enabled(context.Background(), "surface", "") {
		t.Error("nil Evaluator must fall back to WhenUnconfigured")
	}
}

// TestEnabled_UndeclaredKeyFailsClosed pins that the registry is the
// declaration. A typo'd key must not read as on.
func TestEnabled_UndeclaredKeyFailsClosed(t *testing.T) {
	eval := &stubEvaluator{value: true, state: StateResolved}
	r, _ := NewRegistry(eval, rolloutSpec("declared", time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)))

	if r.Enabled(context.Background(), "typo", "u") {
		t.Error("an undeclared key must resolve false")
	}
	if eval.calls != 0 {
		t.Errorf("an undeclared key must not reach the provider, got %d calls", eval.calls)
	}
}

func TestNewRegistry_Validation(t *testing.T) {
	future := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		specs []Spec
		want  error
	}{
		{"empty key", []Spec{{Kind: KindPermanent}}, ErrNoFlagKey},
		{
			"duplicate key",
			[]Spec{{Key: "a", Kind: KindPermanent}, {Key: "a", Kind: KindPermanent}},
			ErrDuplicateFlag,
		},
		{"unknown kind", []Spec{{Key: "a", Kind: "sometimes"}}, ErrUnknownKind},
		{
			"rollout without expiry",
			[]Spec{{Key: "a", Kind: KindRollout, Owner: "platform-team", Ticket: "RFLKT-1"}},
			ErrNoExpiry,
		},
		{
			"rollout without owner",
			[]Spec{{Key: "a", Kind: KindRollout, Ticket: "RFLKT-1", Expires: future}},
			ErrNoOwnerOrTicket,
		},
		{
			"kill switch without ticket",
			[]Spec{{Key: "a", Kind: KindKillSwitch, Owner: "platform-team"}},
			ErrNoOwnerOrTicket,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewRegistry(nil, tc.specs...); !errors.Is(err, tc.want) {
				t.Errorf("NewRegistry err = %v, want %v", err, tc.want)
			}
		})
	}

	t.Run("permanent needs neither owner nor expiry", func(t *testing.T) {
		if _, err := NewRegistry(nil, Spec{Key: "tenant-x", Kind: KindPermanent}); err != nil {
			t.Errorf("NewRegistry: %v", err)
		}
	})
}

// TestExpired_ByKind pins that a rollout past its date is reported and a
// permanent flag never is, however old.
func TestExpired_ByKind(t *testing.T) {
	past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)

	r, err := NewRegistry(nil,
		rolloutSpec("stale-rollout", past),
		rolloutSpec("live-rollout", time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)),
		Spec{Key: "stale-killswitch", Kind: KindKillSwitch, Owner: "platform-team", Ticket: "RFLKT-1", Expires: past},
		Spec{Key: "permanent-with-date", Kind: KindPermanent, Expires: past},
		Spec{Key: "undated-killswitch", Kind: KindKillSwitch, Owner: "platform-team", Ticket: "RFLKT-1"},
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	var got []string
	for _, s := range r.Expired(now) {
		got = append(got, s.Key)
	}
	want := "stale-killswitch,stale-rollout"
	if strings.Join(got, ",") != want {
		t.Errorf("Expired = %v, want %s", got, want)
	}
}

// recorderTB captures AssertNoExpired's failures without failing this test.
type recorderTB struct{ msgs []string }

func (r *recorderTB) Helper()                           {}
func (r *recorderTB) Errorf(format string, args ...any) { r.msgs = append(r.msgs, format) }

// TestAssertNoExpired is the mechanism that makes cleanup non-optional: one
// test in each service, red the day a rollout flag was supposed to be gone.
func TestAssertNoExpired(t *testing.T) {
	past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)

	t.Run("expired rollout fails", func(t *testing.T) {
		r, _ := NewRegistry(nil, rolloutSpec("stale", past))
		rec := &recorderTB{}
		r.AssertNoExpired(rec, now)
		if len(rec.msgs) != 1 {
			t.Errorf("want 1 failure, got %d", len(rec.msgs))
		}
	})

	t.Run("expired permanent passes", func(t *testing.T) {
		r, _ := NewRegistry(nil, Spec{Key: "forever", Kind: KindPermanent, Expires: past})
		rec := &recorderTB{}
		r.AssertNoExpired(rec, now)
		if len(rec.msgs) != 0 {
			t.Errorf("permanent flags must never expire, got %v", rec.msgs)
		}
	})
}

// overrideRegistry builds a registry whose evaluator state the caller picks,
// wrapped in the Override middleware, and reports what Enabled saw.
func overrideRegistry(t *testing.T, state State, target string) (*Registry, http.Handler, *bool) {
	t.Helper()
	r, err := NewRegistry(&stubEvaluator{value: false, state: state},
		rolloutSpec("surface", time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	seen := new(bool)
	h := r.Override(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		*seen = r.Enabled(req.Context(), target, "user:1")
	}))
	return r, h, seen
}

// TestOverride_QueryParamFlipsAndPersists covers the developer ergonomic that
// replaces per-flag switches: one generic param, one cookie, no UI code to add
// when a flag appears or to delete when it goes.
func TestOverride_QueryParamFlipsAndPersists(t *testing.T) {
	_, h, seen := overrideRegistry(t, StateUnconfigured, "surface")

	req := httptest.NewRequest(http.MethodGet, "/?ff_surface=off", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if *seen {
		t.Error("?ff_surface=off must override WhenUnconfigured=true")
	}

	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == overrideCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("the choice must persist as a cookie")
	}
	if cookie.Value != "surface=off" {
		t.Errorf("cookie = %q, want surface=off", cookie.Value)
	}
	// Secure is unconditional: staging is HTTPS and localhost counts as a
	// secure context, so nothing we deploy loses the cookie, and gosec G124
	// stays satisfied without a //nolint.
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("override cookie must be HttpOnly, Secure and SameSite=Lax, got %+v", cookie)
	}
}

// TestOverride_InertWhenProviderResolves is the safety property: the developer
// switch cannot reach a deployment where the provider is authoritative. This is
// structural, not an environment check, so it cannot drift.
func TestOverride_InertWhenProviderResolves(t *testing.T) {
	_, h, seen := overrideRegistry(t, StateResolved, "surface")

	req := httptest.NewRequest(http.MethodGet, "/?ff_surface=on", nil)
	req.AddCookie(&http.Cookie{Name: overrideCookie, Value: "surface=on"})
	h.ServeHTTP(httptest.NewRecorder(), req)

	if *seen {
		t.Error("an override must not beat a provider verdict")
	}
}

// TestOverride_IgnoresUndeclaredKeys keeps the cookie bounded: it is
// client-supplied, so an unfiltered map would be attacker-controlled.
func TestOverride_IgnoresUndeclaredKeys(t *testing.T) {
	r, err := NewRegistry(nil, rolloutSpec("surface", time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	h := r.Override(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/?ff_not-a-flag=on", nil)
	req.AddCookie(&http.Cookie{Name: overrideCookie, Value: "also-not-a-flag=on,surface=off"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	for _, c := range w.Result().Cookies() {
		if c.Name == overrideCookie && strings.Contains(c.Value, "not-a-flag") {
			t.Errorf("undeclared keys must not enter the cookie, got %q", c.Value)
		}
	}
}

// TestOverride_ClearExpiresTheCookie stops a cleared override leaving an empty
// cookie behind that looks like state.
func TestOverride_ClearExpiresTheCookie(t *testing.T) {
	r, _ := NewRegistry(nil, rolloutSpec("surface", time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)))
	h := r.Override(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/?ff_surface=clear", nil)
	req.AddCookie(&http.Cookie{Name: overrideCookie, Value: "surface=off"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	for _, c := range w.Result().Cookies() {
		if c.Name == overrideCookie && c.MaxAge >= 0 {
			t.Errorf("clearing the last override must expire the cookie, MaxAge = %d", c.MaxAge)
		}
	}
}

// TestKeys_SortedForDriftCheck pins the ordering the CI drift check diffs
// against the provider's flag list.
func TestKeys_SortedForDriftCheck(t *testing.T) {
	r, _ := NewRegistry(nil,
		Spec{Key: "zebra", Kind: KindPermanent},
		Spec{Key: "alpha", Kind: KindPermanent},
	)
	got := strings.Join(r.Keys(), ",")
	if got != "alpha,zebra" {
		t.Errorf("Keys = %s, want alpha,zebra", got)
	}

	// Mutating the returned slice must not corrupt the registry.
	r.Keys()[0] = "mutated"
	if r.Keys()[0] != "alpha" {
		t.Error("Keys must return a copy")
	}
}
