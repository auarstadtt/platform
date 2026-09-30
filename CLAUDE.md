# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`github.com/auarstadtt/platform` is the cross-cutting middleware every Digimidi Go
service shares: request correlation, request-scoped logging and rate limiting for
HTTP and gRPC, plus Sentry wiring and a feature-flag registry. It is a **library,
not a service**: no `main`, no config loading, no lifecycle.

It is a fork of Reflekt Lab's `rflkt/platform` at `b2e3e47` (RFLKT-7935). The
README lists what the fork changed. `RFLKT-NNNN` references in comments point at
the upstream tracker and are kept because they name the incident behind a
decision. Nothing syncs from upstream; a fix there is a deliberate cherry-pick.

**This repository is public**, so consumers need no token for `go get`, the Go
quality gate or `docker build`. Never add anything that could not be published:
no client or service names from outside Digimidi, no internal hostnames or IP
addresses, no credentials. Test fixtures use documentation ranges
(`203.0.113.0/24`, `198.51.100.0/24`).

The root package (`package platform`) is flat by design, with no subpackages.

## Commands

```sh
go test ./...                    # whole suite, fast
go test -run TestClientIP ./...  # one test or prefix group
go test -race ./...              # the middleware is concurrent; use before pushing
go vet ./...
```

CI is the "Go quality" ruleset of the `auarstadtt` organisation. It runs
`go-quality.yml` from `auarstadtt/github-workflows` on every pull request:
govulncheck, golangci-lint with the shared config inside that action,
go-arch-lint with this repo's `.go-arch-lint.yml`, and `go test -race`. There is
no workflow file in this repo. To lint locally with the gate's config:

```sh
golangci-lint run --config <github-workflows>/.github/actions/go-quality/golangci.yml
```

Go 1.26 (`go.mod`).

## Architecture

Seven files in the root package, no internal layering:

| File | Owns |
|------|------|
| `requestid.go` | `RequestID` middleware, ID minting/validation, context accessors |
| `logging.go` | `RequestLogger`, `Logger(ctx)`, access log, `statusRecorder` |
| `ratelimit.go` | `RateLimit`, the `Limiter` interface, `memoryLimiter`, `ClientIP` |
| `grpc.go` | `UnaryServerInterceptor`, `OutgoingRequestID` (correlation across hops) |
| `sentry.go` | `InitSentry`, `ShutdownSentry`, `UseSentry`, `Recoverer`, the `Router` interface |
| `flags.go` | `Spec`, `Kind`, `State`, `Evaluator`, `Registry`, `AssertNoExpired`, the `Override` middleware |
| `chain.go` | package doc, `Options`, `Wrap`, `ResolveHealthPath`, `ExemptPaths` |

### Feature flags: three states, not a boolean (RFLKT-6035)

`flags.go` exists because retiring a flag upstream (RFLKT-6033) was dangerous for a
non-obvious reason. That service resolved a flag's default through one `fallback`
argument that applied only when flag evaluation was *unconfigured*. Production
configures evaluation, so a flag the provider's dashboard no longer defined
resolved **false** there. Deleting the flag in the dashboard before deleting the
code that read it flipped production onto the off branch. Two genuinely different
situations were both spelled `false` and the caller could not tell them apart.

So `Evaluator.Evaluate` returns a `State` (`StateUnconfigured`, `StateUndefined`,
`StateResolved`) and a `Spec` declares `WhenUnconfigured` and `WhenUndefined`
separately. That buys **graduation**: a flag at 100% rollout sets
`WhenUndefined: true`, and after that deploy the dashboard definition and the code
can be deleted in either order. Retirement stops being a sequenced operation,
which was the actual risk.

`Evaluator` is an interface rather than a PostHog client for the dependency-surface
reason below — the service supplies the binding and `go.mod` does not move.

### Two composition styles, both first-class

`Wrap(next, Options{...})` builds the fixed chain `RequestID → RequestLogger → RateLimit`.
The order is not configurable: RequestID must precede the logger so records carry the ID, and
the logger must precede the limiter so 429s are attributable.

**Most real services cannot use `Wrap`** — they interleave their own middleware (Sentry, CORS,
auth) and rate-limit *after* auth so the key is the user identity rather than a shared office
NAT IP. So every middleware must stay usable standalone, with the same defaults it would get
through `Wrap`. When you add an ergonomic to `Options`, add it to
`LoggingOptions`/`RateLimitOptions` too and have `Wrap` forward it down — upstream once put
`HealthPath` only on `Options`, which made it unreachable for exactly the services it was meant
to help. Tests named `TestALaCarte_*` cover this axis; keep them growing alongside the `Wrap`
tests.

`HealthPath` (default `/health`, `"-"` disables) is the pattern: one declaration applies two
unrelated exemptions — no rate limiting (a throttled probe reads as an unhealthy pod and gets
it restarted) and no access-log record (it fires forever).

### Invariants worth not breaking

- `Logger(ctx)` never returns nil; the fallback is `zap.NewNop()`, deliberately silent so a
  missing middleware shows up as absent logs rather than uncorrelated ones.
- `validInboundID` (`[A-Za-z0-9._-]`, ≤64 chars) is shared by the HTTP and gRPC paths — the ID
  is echoed in a response header and written to structured logs, so this prevents header and
  log injection at both hops.
- Zero-value `RateLimitOptions` is a pass-through: limiting is opt-in.
- An explicitly-set `Exempt` wins outright over `HealthPath` rather than being merged.
- `memoryLimiter` is bounded (`maxKeys`) and cleared wholesale when exceeded — the keyspace is
  attacker-controlled, so unbounded growth would make the limiter the outage.
- `ClientIP` reads `X-Forwarded-For` from the RIGHT. It drops the Google front-end hops that
  Traefik appends, then the load balancer's own address, and returns the next entry.
  Everything left of that is client-supplied. The upstream rule took the left-most entry, and
  behind the GKE platform's Google load balancer that let any client choose its own rate-limit
  bucket. The header shape is pinned by `TestClientIP_ReadsTheChainFromTheRight`.
  `googleFrontEnds` must match the ranges Traefik trusts (`forwarded_headers_trusted_ips`
  in the platform's Traefik module); if Traefik trusts a range this list lacks, every client
  shares the load balancer's bucket.
- `ShutdownSentry` flushes **and then closes**. On sentry-go v0.47 `Flush` returns the
  telemetry processor's result without waiting on the transport, so it reports success on
  events that never arrive; `Close` is the deterministic drain. Verified by deliberately
  removing the `Close` call — `TestShutdownSentry_ClosesTheClientNotJustFlush` fails.
- `UseSentry` mounts `Recoverer` outermost and `sentryhttp` with `Repanic: true` inside it.
  Reversed, 0 events are reported; without `Repanic`, a panicked request returns 200. Both
  verified the same way. `Repanic` is deliberately not configurable.
- `SentryActive` reads the hub rather than a flag `InitSentry` sets, so the state cannot
  drift. `InitSentry`'s empty-DSN short-circuit is load-bearing: `sentrygo.Init` accepts an
  empty DSN and binds a discarding client, which would look configured to `UseSentry`.
- `Registry.Enabled` consults developer overrides **only** in `StateUnconfigured`. That is
  what makes `Override` inert in production structurally, rather than via an environment
  check that could drift. Do not "helpfully" honour an override in the other states.
- An undeclared key resolves `false` and never reaches the `Evaluator`. The registry is the
  declaration, so a typo'd key is a bug and must fail closed rather than open.
- `Override` filters both the query param and the cookie against declared keys. The cookie is
  client-supplied; unfiltered it would be an unbounded attacker-controlled map.
- The override cookie is unconditionally `Secure`. Overrides only apply in
  `StateUnconfigured` (staging over HTTPS, or localhost, which browsers treat as a secure
  context), so nothing loses the cookie, and it satisfies gosec G124 without a `//nolint`.
- `Expired` takes `now` rather than calling `time.Now`, so `AssertNoExpired` is deterministic
  and both the expired and not-yet-expired branches are testable.
- `TB` is declared locally rather than importing `testing`, which would otherwise be linked
  into every service's production binary.

### Dependency surface is the real constraint

This module is imported by every Go service, so anything added to `go.mod` lands in all of
them. Keep it to `zap`, `golang.org/x/time`, `grpc`, and `getsentry/sentry-go`. No router, no
ORM, no cloud SDK. This is guarded by review, not by a linter: `.go-arch-lint.yml` is a
deliberate override explaining why the organisation's layer graph cannot apply to a library
this shape. Read it before touching the gate.

The router exclusion is why `UseSentry` takes the local one-method `Router` interface rather
than `chi.Router`, and why `Recoverer` is implemented here rather than borrowed from
`chi/middleware`: gRPC-only consumers would otherwise carry a router for nothing. Keeping our
own `Recoverer` also means the panic record goes through `Logger(ctx)` and carries the request
ID, which chi's does not.

### Out of scope, on purpose

Routing, authentication, authorization, CSRF, tenant scoping, and gRPC rate limiting (it keys
on peer identity and belongs with the service's auth interceptor). Those are service-specific
policy. Also out of scope within Sentry: tracing, profiling, and log/metric batching.

Upstream has `oauth/` (a resource-server middleware for its own identity provider) and `mcp/`
(an MCP endpoint boilerplate). The fork dropped both because nothing here consumes them.
Bring one back from upstream when a Digimidi service needs it, rather than rebuilding it.

## Style

Comments here explain *why*, often citing the concrete incident or ticket that forced the
decision (see `ratelimit.go`'s `ClientIP`, `logging.go`'s `HealthPath`). Match that when adding
code: a reader should be able to tell which constraint a design choice is paying for. Commit
messages follow the same convention.
