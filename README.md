# platform

The standard cross-cutting middleware for Digimidi Go services: request
correlation, request-scoped logging, rate limiting, error reporting, and
feature flags. It is wired once, the same way, in every service.
[`go-service-template`](https://github.com/auarstadtt/go-service-template)
starts every new service with it already mounted.

    go get github.com/auarstadtt/platform

This repository is public so that `go get`, the Go quality gate and image
builds need no token. It holds no secrets and no client logic.

## Where this comes from

This is a fork of Reflekt Lab's `rflkt/platform` at `b2e3e47`, taken for
RFLKT-7935. Ticket references in comments (`RFLKT-NNNN`) point at the
upstream tracker. They are kept because each one names the incident that
forced the decision beside it.

It exists because concerns like these get re-implemented per repository, or
silently skipped: loggers reached as package globals that no request ID can
reach, services with no rate limiting at all, Sentry depended on but never
initialised. One shared chain turns correlation, limiting and error reporting
into defaults rather than things to remember.

What changed in the fork, and why:

| Change | Why |
|--------|-----|
| `ClientIP` reads `X-Forwarded-For` from the right | Behind the GKE platform's Google load balancer the left-most entry is whatever the client sent. The upstream rule let any client choose its own rate-limit bucket. See `ratelimit.go`. |
| `oauth/` removed | It verifies tokens from the upstream identity provider. Digimidi has no such issuer. |
| `mcp/` removed | It has no consumer here. Untested surface is a liability in a shared library. Restore it from upstream when a Go MCP server needs it. |
| `.no-sentry` removed | It exempted a Sentry gate that exists only upstream. |

Nothing is kept in sync with upstream automatically. A fix made there is a
deliberate cherry-pick here.

## Usage

    logger, _ := zap.NewProduction()

    h, err := platform.Wrap(router, platform.Options{
        Logger:      logger,
        ServiceName: "nps-web",
        RateLimit:   platform.RateLimitOptions{RequestsPerSecond: 20, Burst: 40},
    })
    if err != nil { log.Fatal(err) }

    http.ListenAndServe(":8080", h)

Inside a handler, always take the logger from the context — it carries the
request ID:

    platform.Logger(ctx).Info("booking created", zap.String("ref", ref))

### Composing a la carte

Most services cannot hand their whole chain to `Wrap` — they interleave their
own middleware (Sentry, CORS, auth), and rate limiting often has to run *after*
auth so it can key on the user rather than the IP. Mount the pieces directly:

    r.Use(platform.RequestID)
    r.Use(platform.RequestLogger(platform.LoggingOptions{
        Logger: logger, ServiceName: "nps-web", AccessLog: true,
    }))
    r.Use(mySentry, myRecoverer, myCORS, myAuth)

    // After auth, so the key is the identity rather than a shared office IP.
    r.Use(platform.RateLimit(platform.RateLimitOptions{
        RequestsPerSecond: 20, Burst: 40,
        KeyFunc: func(r *http.Request) string {
            if u := auth.FromContext(r.Context()); u != nil { return u.Email }
            return platform.ClientIP(r)
        },
    }))

The health probe is exempt from limiting and skipped in the access log by
default in both forms — `HealthPath` (default `/health`, `"-"` to disable) is on
`LoggingOptions` and `RateLimitOptions`, not only on `Options`.

For gRPC services — the service name tags logs here too, so pass it as well:

    grpc.NewServer(grpc.ChainUnaryInterceptor(
        platform.UnaryServerInterceptor(logger, "my-grpc-service"),
    ))

## Error reporting

Wire Sentry at boot, mount the panic chain on the router, drain on shutdown:

    platform.InitSentry(platform.SentryOptions{
        DSN:         cfg.SentryDSN,
        Environment: cfg.Environment,
        Release:     cfg.SentryRelease,
        Logger:      logger.Named("sentry"),
    })
    defer platform.ShutdownSentry(platform.DefaultSentryShutdownTimeout)

    platform.UseSentry(r, platform.SentryHTTPOptions{})

`UseSentry` takes the one-method `platform.Router` interface, which `chi.Router`
satisfies as-is — platform does not import a router. It mounts `Recoverer`
outermost and, when Sentry is configured, `sentryhttp` with `Repanic` inside it.
That order is a correctness property, not a preference, so it is not
configurable: reverse it and a panic reports 0 events; drop `Repanic` and a
request that panicked returns HTTP 200, so a failed save looks successful.
Without a DSN only `Recoverer` is mounted, so a keyless deploy still turns a
panic into a 500 with the request ID attached.

Everything is fail-soft. An empty DSN is a no-op and a malformed one logs and
continues — local dev, `go test` and any keyless deploy boot unchanged.

`ShutdownSentry` flushes **and then closes**. On sentry-go v0.47 `Flush` alone
does not reliably deliver: events route through a telemetry Processor and
`Flush` returns that processor's result without ever waiting on the transport,
so it can report success on events that never arrive. `Close` is the
deterministic drain. Do not "simplify" this to a flush —
`TestShutdownSentry_ClosesTheClientNotJustFlush` exists to fail if you do.

### Feature flags

Declare every flag in one registry. The `Evaluator` is an interface, so the
service brings its own provider binding and this module gains no dependency:

    var Flags, _ = platform.NewRegistry(myPostHogBinding{client},
        platform.Spec{
            Key: "new-checkout", Kind: platform.KindRollout,
            Owner: "platform-team", Ticket: "RFLKT-7935",
            Expires:          time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
            WhenUnconfigured: true,  // visible in staging, dev and tests
            WhenUndefined:    false, // dark if the dashboard has no definition
        },
    )

    if Flags.Enabled(ctx, "new-checkout", "user:"+id) { ... }

`WhenUnconfigured` and `WhenUndefined` are separate on purpose. Unconfigured
means evaluation is not running (staging, dev, tests). Undefined means it *is*
running and the provider has no definition for the key, which is what
production looks like after someone deletes the flag in the dashboard. Before
this split those were both a single `fallback` argument, and deleting a flag
ahead of the code that read it silently flipped production onto the off branch
(RFLKT-6033).

**Retiring a flag.** Set `WhenUndefined: true` and deploy. The flag is now
*graduated*: an absent provider definition resolves the same way a fully rolled
out one does, so the dashboard entry and the code can be deleted in either
order, with no window where production reads the wrong branch.

**Expiry is enforced, not suggested.** A `KindRollout` spec must carry an
`Expires` date, and one test per service turns the build red once it passes:

    func TestFlagsNotExpired(t *testing.T) {
        flags.Flags.AssertNoExpired(t, time.Now())
    }

`KindPermanent` never expires; `KindKillSwitch` expires only if you date it.

**Overrides are generic.** `Flags.Override` middleware honours
`?ff_<key>=on|off|clear`, persisted in one `ff_overrides` cookie, for any
declared flag. Adding a flag needs no UI code and removing one needs no UI code
— the per-flag cookie, query param and sidebar toggle that one upstream flag
accumulated cost more to delete than the flag itself. Overrides apply only in
`StateUnconfigured`, so the switch cannot reach a deployment where the provider
is authoritative.

## Scope

Deliberately does NOT wrap: routing, authentication, authorization, CSRF, or
tenant scoping. Those are service-specific policy. This module owns only the
concerns that must be identical everywhere to be useful.

Within feature flags specifically: this owns the registry, the lifecycle and the
resolution rules, not the provider. Which SDK evaluates a flag, how it is
polled, and what a distinct ID means are service concerns behind `Evaluator`.
