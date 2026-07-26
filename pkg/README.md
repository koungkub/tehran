# pkg — shared service infrastructure

Importable building blocks for a Go service. Everything here is transport and
telemetry plumbing; no business logic, and nothing specific to the service that
happens to live alongside it in this repository.

```go
import (
    "github.com/koungkub/tehran/pkg/connectrpc"
    "github.com/koungkub/tehran/pkg/lifecycle"
    "github.com/koungkub/tehran/pkg/ops"
    "github.com/koungkub/tehran/pkg/telemetry"
)
```

| Module | What it gives you |
|---|---|
| `connectrpc` | An RPC server serving the Connect, gRPC and gRPC-Web protocols on one port, with logging, tracing, metrics, panic recovery, gRPC health and reflection already wired |
| `ops` | `/metrics`, `/healthz` and `/readyz` on a separate port, with named readiness checks |
| `lifecycle` | Runs components concurrently and stops them in reverse registration order, with signal handling and a shutdown backstop |
| `telemetry` | OTLP tracing, Prometheus-bridged metrics, and a `*slog.Logger` whose records carry the active `trace_id` and `span_id` |

## Using it

```go
tel, err := telemetry.Setup(ctx, telemetry.Config{
    ServiceName:    "billing",
    ServiceVersion: version.Version, // your ldflags stamp
    Enabled:        true,
    Endpoint:       "localhost:4317",
    Insecure:       true,
    SampleRatio:    1.0,
})
log, err := telemetry.NewLogger(telemetry.LogConfig{Level: "info", Format: "json"})

rpc, err := connectrpc.New(connectrpc.Config{Host: "0.0.0.0", Port: 8080},
    connectrpc.WithModules(billingAPI),  // anything with a Register method
    connectrpc.WithLogger(log),
    connectrpc.WithTracerProvider(tel.TracerProvider),
    connectrpc.WithMeterProvider(tel.MeterProvider),
)

opsSrv := ops.New(ops.Config{Host: "0.0.0.0", Port: 9090},
    ops.WithRegistry(tel.PromRegistry),
    ops.WithReadyCheck("drain", drainCheck),
)
```

Both servers own their lifecycle: `Serve(ctx)` blocks, and cancelling `ctx`
drains in-flight requests within the configured `ShutdownTimeout` before
returning. Hand them to a supervisor to have that sequenced:

```go
sup := lifecycle.New(lifecycle.Config{ShutdownTimeout: 30 * time.Second},
    lifecycle.WithLogger(log),
    lifecycle.WithComponents(opsSrv, rpc),   // start order; shutdown is the reverse
    lifecycle.BeforeShutdown(func() { ready.Store(false) }),
)
err := sup.Run(ctx)   // blocks until SIGTERM, ctx, or a component exiting
```

**Registration order is load-bearing.** Shutdown runs in reverse, so register
the components others depend on *first* and they stop *last*. Above, `ops` is up
before `rpc` and keeps answering `/readyz` and `/metrics` for the whole time
`rpc` is draining. Swap the two and the probe port closes first, leaving an
orchestrator blind exactly when it most needs to see the instance leaving
service. `TestStopsInReverseRegistrationOrder` fails if that reverses.

Anything with `Name() string` and a blocking `Serve(context.Context) error` is a
component — that is the whole interface, and neither server imports `lifecycle`
to satisfy it. `BeforeShutdown` hooks run while every port is still open, which
is what makes the readiness flip observable. `Config.ShutdownTimeout` is a
backstop over the entire sequence, not a per-component drain budget: exceed it
and `Run` returns an error naming every component still running.

Mounting handlers needs no import of `connectrpc` at all. A type satisfies
`connectrpc.Module` structurally:

```go
func (h *Handler) Register(mux *http.ServeMux, opts ...connect.HandlerOption) []string {
    mux.Handle(billingv1connect.NewBillingServiceHandler(h, opts...))
    return []string{billingv1connect.BillingServiceName}   // for health + reflection
}
```

## The rules these modules follow

Four constraints are what keep this a library instead of a folder of code to
copy. They are worth honouring in anything added here.

**Never import `internal/`.** Go does not enforce this — `internal/` is
importable from anywhere in the same module, `pkg/` included — so the
`pkg-no-internal` depguard rule in `.golangci.yml` is the only thing that
catches a violation. Run the linter, not just the compiler.

**Own your config struct.** Each module declares its own `Config` with its own
`mapstructure` tags. The application composes them; no module imports a central
config package, because that would force this service's configuration shape and
its `TEHRAN_` environment prefix onto every consumer. Anything that cannot come
from a config file — a build stamp, say — is a field the application assigns, not
something the module reaches for.

**Take interfaces, not implementations.** `connectrpc` accepts
`trace.TracerProvider` and `metric.MeterProvider`, not `*telemetry.Telemetry`.
That is why the RPC server can be used with no telemetry at all, and why
`connectrpc-no-telemetry` in `.golangci.yml` forbids the shortcut.

**`*slog.Logger` at the boundary.** No module exposes a back-end logger type, so
consumers are not made to adopt one. `slog` is the front end; zerolog is the back
end, via `zerolog.NewSlogHandler`, and that is an implementation detail of
`telemetry.NewLogger` alone.

## Logging specifics

Fields follow **zerolog's** names, not slog's: `level`, `message`, `time`.
Renaming them means assigning to zerolog's package-level variables
(`zerolog.MessageFieldName` and friends), which would reach into every other use
of zerolog in the importing process, so they are left alone.

`log.level` is a **zerolog** level name — `trace`, `debug`, `info`, `warn`,
`error`, `fatal`, `panic`. An unknown name is a startup error.

Groups **flatten to dotted keys** (`logger.WithGroup("req")` yields `req.path`),
which is zerolog's convention rather than slog's nested objects.
`trace_id`, `span_id` and `caller` stay at the top level regardless of any open
group — that is the whole reason `CorrelationHandler` replays groups itself
instead of pushing them into the back end.

`caller` exists because zerolog's slog handler ignores `Record.PC` entirely.
`CorrelationHandler` resolves the program counter slog captured at the call site,
so the frame points at your code rather than at the logging plumbing.

`telemetry.NewLogger` installs its logger as the `slog` default, which also
routes the standard library's `log` package through it.

Correlation is read from the context, so it only works if you pass one. Use
`log.InfoContext(ctx, ...)` or `log.LogAttrs(ctx, ...)`; plain `log.Info(...)`
produces a line with no `trace_id`. For the same reason the otel interceptor is
registered ahead of the logging one inside `connectrpc.New` — Connect makes the
first interceptor outermost, so the reverse order would leave every RPC log line
uncorrelated. `TestRPCLogLineSeesSpanContext` exists to keep that from
regressing.
