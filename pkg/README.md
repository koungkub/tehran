# pkg — shared service infrastructure

Importable building blocks for a Go service. Everything here is transport and
telemetry plumbing; no business logic, and nothing specific to the service that
happens to live alongside it in this repository.

```go
import (
    "github.com/koungkub/tehran/pkg/connectrpc"
    "github.com/koungkub/tehran/pkg/database"
    "github.com/koungkub/tehran/pkg/lifecycle"
    "github.com/koungkub/tehran/pkg/migrate"
    "github.com/koungkub/tehran/pkg/ops"
    "github.com/koungkub/tehran/pkg/telemetry"
)
```

| Module | What it gives you |
|---|---|
| `connectrpc` | An RPC server serving the Connect, gRPC and gRPC-Web protocols on one port, with logging, tracing, metrics, panic recovery, gRPC health and reflection already wired |
| `ops` | `/metrics`, `/healthz` and `/readyz` on a separate port, with named readiness checks |
| `database` | A GORM handle over a tuned connection pool, with a bounded startup connect, per-statement logs and spans, pool metrics, and a readiness probe |
| `migrate` | Versioned SQL migrations over goose, embedded in the binary, with concurrent runners locked apart and a half-finished run reporting what it applied |
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

db, err := database.Open(ctx, database.Config{
    Host: "127.0.0.1", Port: 5432, User: "svc", Database: "billing",
    Password: os.Getenv("BILLING_DB_PASSWORD"),
},
    database.WithLogger(log),
    database.WithTracerProvider(tel.TracerProvider),
    database.WithMeterProvider(tel.MeterProvider),
)

opsSrv := ops.New(ops.Config{Host: "0.0.0.0", Port: 9090},
    ops.WithRegistry(tel.PromRegistry),
    ops.WithReadyCheck("drain", drainCheck),
    ops.WithReadyCheck("database", db.Ping),
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

## Timeouts

One rule decides most of the timeouts on the RPC server: **a timeout that bounds
a total duration eventually kills a healthy stream, so the safe ones bound a
_lack of progress_.** A streaming RPC is legitimately long-lived, so any fixed
ceiling on it is a bomb with a long fuse; a progress-based timeout fires only
once things have genuinely gone quiet, which is what "broken" actually looks
like.

That is why `net/http`'s `WriteTimeout` is **never set** — it covers the entire
`ServeHTTP` lifetime, a streaming response included. Two gaps that rule leaves
cannot be reached by anything progress-based, so they get scoped exceptions:
`request_timeout` and `read_timeout`.

| Setting | Fires when | Default |
|---|---|---|
| `read_header_timeout` | headers take too long to arrive (Slowloris) — **HTTP/1.1 only** | 10s |
| `idle_timeout` | a kept-alive connection has no request in flight | 120s |
| `keepalive_interval` | nothing has arrived, so an HTTP/2 PING checks the peer is alive | 30s |
| `write_byte_timeout` | writes stop making progress (a client that stopped reading) | 30s |
| `request_timeout` | a **unary** handler runs too long with no deadline of its own | 15s |
| `read_timeout` | one request's body never finishes arriving | **off** |
| `max_concurrent_streams` | a single **connection** opens too many streams | 250 |

`keepalive_interval` is the important one to understand: with no `ReadTimeout`, a
peer that vanishes **without a FIN** — a partition, a crashed host, a NAT
dropping the flow — would otherwise hold its streams and their goroutines until
the OS gives up on the socket, which can take hours. It maps to
`http.HTTP2Config.SendPingTimeout` (stdlib since Go 1.24; no `x/net/http2`
import), and `Server.HTTP2` is honoured on the h2c path.

Two entries in that table come with caveats worth knowing before you rely on
them:

- **`read_header_timeout` does nothing on the h2c path.** Go's HTTP/2 server
  never reads the field (`grep -c 'hs.ReadHeaderTimeout' h2_bundle.go` is `0`);
  it uses its own hardcoded handshake timeouts. So the Slowloris guard covers
  HTTP/1.1 Connect clients and not gRPC clients. `read_timeout` is the equivalent
  there. On the HTTP/1.1 path the two also have to be ordered: `net/http` arms
  the header phase with `read_header_timeout` and only swaps to the
  `read_timeout` deadline once the headers are read, so
  `read_header_timeout <= read_timeout` whenever the latter is on — otherwise the
  header phase outlives the whole-request budget and the body phase begins on an
  expired deadline.
- **`max_concurrent_streams` bounds a connection, not a caller.** The default is
  the standard library's own current default, so it changes nothing until you
  lower it, and nothing here caps how many connections one peer may open.

`request_timeout` covers a handler that hangs while the connection is perfectly
healthy: the peer answers PINGs, a stream is open so `idle_timeout` cannot fire,
and a handler that has written nothing has no data available to write, so
`write_byte_timeout` has not even started counting. A context deadline is also
the better instrument than `WriteTimeout` would be — it cancels the handler's
work and returns a real `CodeDeadlineExceeded` instead of tearing the connection
down underneath it.

`read_timeout` covers the body-read phase, which **nothing else reaches**.
Connect decodes the request message *before* the interceptor chain runs
(`handler.go`: `receiveUnaryRequest` then `untyped(ctx, request)`), so
`request_timeout`'s clock has not started yet; the HTTP/2 idle timer is stopped
the moment a stream opens; `write_byte_timeout` has nothing to write; and a peer
that dribbles its body while answering PINGs defeats `keepalive_interval`. Left
off, such a client pins a stream and its handler goroutine indefinitely — a
slow-body attack with 250 streams available per connection.

It is off by default because it is the one setting that is not safe for every
service, but it is narrower than its `net/http` reputation: under HTTP/2 it is
armed **per stream** after the headers are read and closes only that stream's
request body. That makes it safe for unary and server-streaming RPCs, and unsafe
for client-streaming or bidi, where a long upload is the whole point. **Turn it
on if no registered module streams _from_ the client.**

Three things about it are deliberate:

- **A deadline the caller sent always wins**, in either direction. Connect has
  already turned `Connect-Timeout-Ms` / `grpc-timeout` into a context deadline
  before the interceptor runs, and overriding what a caller explicitly asked for
  would be wrong — the same stance grpc-go takes. So a client *may* ask for
  longer than `request_timeout`.
- **Streaming handlers are exempt**, because they are meant to run long.
  `TestTimeoutInterceptorLeavesStreamsAlone` fails if that changes.
- **There is no off switch.** A non-positive value means "use the default", as
  everywhere else in these `Config`s, and a unary handler with no deadline at all
  is the failure mode this exists to prevent. A service with legitimately long
  unary RPCs raises the value.

It bounds a handler that *observes* its context — anything doing I/O through a
database driver or an HTTP client does. A handler that ignores its context runs
to completion regardless; no server-side timeout can preempt one without leaking
the goroutine it leaves behind.

### The shutdown timeouts are chosen together

`shutdown_timeout` relies on `http.Server.Shutdown`, and it is worth being exact
about what that does: it closes listeners, sends the HTTP/2 graceful `GOAWAY`,
then **polls for connections to become idle**. It never cancels a handler's
context and never closes an active connection. On timeout it just returns
`ctx.Err()` while the handlers keep running.

So a drain shorter than a request is not a graceful drain — it is a request cut
off by process exit, and `Serve` returning an error means the supervisor returns
one too and the process exits **non-zero on an ordinary deploy**. Three
independently-owned settings therefore have to line up:

```
read_timeout + request_timeout  <=  server.shutdown_timeout
server.shutdown_timeout + ops.shutdown_timeout  <=  lifecycle.shutdown_timeout
lifecycle.shutdown_timeout  <  the orchestrator's grace period
```

The first line is additive because the phases are sequential: Connect reads and
decodes the request message, and only then runs the handler. The defaults satisfy
it (`0 + 15s <= 20s`, since `read_timeout` is off), as does this repository's
`config.toml` with it enabled (`5s + 15s <= 20s`, `20s + 5s = 25s < 30s`). The
last line is yours: with a 30s `lifecycle.shutdown_timeout`, Kubernetes
`terminationGracePeriodSeconds` should be **35 or more**, not the 30s default.
Components drain one at a time in reverse order, so the supervisor's budget has
to cover their *sum*, not their maximum.

Each of these values is defensible on its own and only the combination is wrong,
which is why no single package's tests would catch it.
`TestShutdownTimeoutsAreCoherent` in `internal/config` and the pairing check in
`connectrpc`'s `TestConfigDefaults` both fail if the relationship breaks.

A streaming RPC in mid-flight never goes idle either, so a service with
long-lived streams has to raise `shutdown_timeout` or cancel those streams
itself, or the `lifecycle` backstop will report the server as still running.

## The database

`database.Open` hands back a pool and a GORM handle over it. Domain code sees
only the handle:

```go
func (r *AccountRepository) Find(ctx context.Context, id string) (*Account, error) {
    var a Account
    err := r.db.WithContext(ctx).First(&a, "id = ?", id).Error   // r.db is db.Gorm()
    ...
}
```

`WithContext(ctx)` is not optional. It is what puts the statement's span under the
request's span and the request's `trace_id` on the statement's log line; without
it both still happen, unattached to anything.

**One pool per process, closed last.** The pool is the service's budget of
connections to that server, so every repository shares one — a pool per domain
multiplies the budget by the number of domains. `DB` is deliberately *not* a
`lifecycle.Component`: there is nothing to serve, and a component would be stopped
partway through the shutdown sequence, pulling the pool out from under handlers
that are still draining. Close it after the supervisor returns instead.

`Open` also does two things GORM does not. It disables GORM's automatic ping,
which takes no context and would block startup for as long as the driver's dial
allows, and pings itself under `connect_timeout`. And a failed ping closes the
pool before returning the error, because `gorm.Open` has already opened one by
then and dropping the handle would leak it —
`TestOpenClosesThePoolOnAFailedPing` and
`TestOpenDoesNotHangOnAnUnreachableDatabase` fail if either changes.

`Ping` is meant to be handed to `ops.WithReadyCheck` as-is, and it does not bypass
the pool: with every connection busy it waits for one, which is the pool's health
as a caller experiences it. That is why it imposes `connect_timeout` on a context
carrying no deadline — an HTTP request's context does not. Without it a probe
against an exhausted pool or an unresponsive server would sit in the handler for
as long as the prober waited, and an orchestrator that gets no reply cannot tell
"not ready" from "not answering". A deadline the caller *did* set always wins, in
either direction, the same stance `connectrpc` takes on a timeout a client sent
(`TestPingBoundsADeadlinelessContext`, `TestPingKeepsTheCallersDeadline`).

### Pool sizing is the part worth reading

`database/sql` blocks a caller when no connection is free, so the pool is a queue
in front of the database. That framing decides every setting here.

| Setting | Fires when | Default |
|---|---|---|
| `max_open_conns` | connections in use and idle together reach the cap; further callers wait | 25 |
| `max_idle_conns` | a connection is returned to an already-full idle set, and is closed | = `max_open_conns` |
| `conn_max_lifetime` | a connection reaches this age and is retired | 30m |
| `conn_max_idle_time` | a connection has gone unused for this long | 5m |
| `connect_timeout` | a new connection takes too long to dial | 5s |
| `slow_query_threshold` | a statement takes longer, and is logged at warn | 200ms |

`max_open_conns` is **per process**, and each replica opens its own pool, so the
server's `max_connections` has to cover it times the replica count with headroom
for migrations and operators. A saturated pool does not error — it queues — so it
surfaces as callers timing out while `db.client.connection.wait.count` climbs.

`max_idle_conns` defaulting to `max_open_conns` is deliberate: `database/sql`
closes any connection returned to a full idle set, so a lower idle limit makes a
service at its peak open and close connections continuously. Two metrics tell
these apart — `wait.count` climbing means raise `max_open_conns`, whereas
`closed{reason="idle"}` climbing means raise `max_idle_conns`.

`conn_max_lifetime` is what picks up a failover, a DNS change or a rolling
restart of the database without restarting this process. Keep it below any
idle-connection timeout the database or a proxy in front of it enforces, so this
side retires connections before the other end kills them.

One relationship spans two packages: the pool opens connections lazily, so a
request arriving when none is free waits for one to be dialled **out of its own
deadline**. So

```
database.connect_timeout  <  server.request_timeout
```

or a request that has to open a connection cannot succeed — it exhausts its
budget waiting for the connection it needs. `TestDatabaseConnectTimeoutFitsARequest`
in `internal/config` checks the shipped values.

`prepare_stmt` is off by default although it is faster: prepared-statement
caching assumes consecutive statements reach the same server-side session, which
a pooler in transaction or statement mode (PgBouncer, RDS Proxy) does not
guarantee.

### Statements, and the level they land at

There is one log line per statement, and its level is a classification rather
than a formality — the same stance `connectrpc` takes on an RPC's `code`. Two of
these outcomes are not failures, and logging them as errors is what teaches an
on-call rotation to ignore database errors.

| Line | Level | Means |
|---|---|---|
| `sql` | debug | it ran |
| `sql not found` | debug | `First` on an empty set: the documented result, not a fault |
| `sql aborted` | warn | the caller cancelled or ran out of time |
| `sql slow` | warn | over `slow_query_threshold` |
| `sql failed` | error | it genuinely failed |

Every line carries `statement`, `duration` and `rows`. At the default level the
debug ones cost nothing: GORM defers building the SQL and counting the rows to a
closure, and it is only called once the level is known to be enabled
(`TestStatementTextIsNotBuiltWhenItWouldNotBeLogged`). `db.Debug()` on a single
query promotes its statement to info, so debugging one query does not mean
raising the level of the whole process.

`caller` points at the repository that ran the query, not at this package. GORM
calls the logger from inside its own callback chain, so the records are built by
hand to resolve the frame below both GORM and this module
(`TestCallerPointsAtTheCallingCode`).

### Values stay out of logs and traces

`include_query_values` is off by default, and it is the one setting here with a
privacy consequence: bound parameters *are* the data — e-mail addresses, tokens,
national identifiers. Left off, both the log line and the span carry the
statement with its placeholders intact, which is what a query plan needs anyway
(`TestSpanKeepsBoundValuesOut`). Turn it on locally, not in production.

Nothing in this module logs or exports a DSN either. Errors, log lines and span
attributes name `Config.Target()`, which is credential-free by construction:
built from the fields it omits the password, and given a whole DSN it reports only
the host and database it can *prove* it read.

That proof matters more than it sounds, because `url.Parse` accepts far more than
URLs. A libpq key/value DSN (`host=… password=…`) parses with no error at all and
lands whole — password included — in `Path`, and a MySQL DSN parses to an opaque
with nothing usable in it. So a DSN is only picked apart when the parse yields a
host, and otherwise reports just the driver name. Taking either at face value
would have published the credentials as `db.namespace`, where a trace backend
would then index them;
`TestConnectionAttrsNeverCarryCredentials` and `TestTargetNeverCarriesCredentials`
fail if that guard goes.

### Spans and metrics

A statement's span is named `{operation} {table}` — `SELECT widgets` — which is
the convention a tracing backend groups on. The table is not known when the span
starts, since GORM resolves it while building the SQL, so the span is renamed in
the after-callback rather than started with a fixed name. It carries
`db.system.name`, `db.namespace`, `db.operation.name`, `db.collection.name`,
`server.address`, `server.port`, `db.query.text` and the row count.

`db.system.name` comes from the dialector, not from `Config.Driver`: a connection
opened through `WithDialector` ignores that field, so reporting it would label a
SQLite or Spanner connection as whatever `Config` happened to default to
(`TestSystemNameFollowsTheDialector`).

`gorm.ErrRecordNotFound` does not set an error status and does not produce an
`error.type`, for the same reason it logs at debug: a trace that flags every empty
lookup as an error makes an error-rate panel useless
(`TestMissingRowIsNotASpanError`).

The span is ended by the callback that started it, tracked through the
statement's own context. Reading it back with `trace.SpanFromContext` would
return the *caller's* span whenever the before-callback had not run, and ending
that would truncate the request's trace at its first query —
`TestStatementSpanJoinsTheCallersTrace` covers both halves, and
`TestStatementSpansStaySiblings` pins the shape across a multi-statement
transaction, where every statement hangs off the request rather than off the
statement before it.

Metrics are `db.client.operation.duration` per statement plus the pool, observed
through `database/sql`'s own statistics. The instruments are observable, so
nothing is measured until a scrape asks, and every value in one scrape comes from
a single `Stats()` call and is therefore self-consistent.
`db.client.connection.count`, `.max` and `.idle.max` are the OpenTelemetry
semantic convention; `.wait.count`, `.wait.duration` and
`.closed{reason=idle|idle_time|lifetime}` have no convention to follow and keep to
its prefix. `Close` unregisters the callback, which otherwise holds a closed pool
and keeps reading it on every scrape (`TestCloseStopsPoolMetrics`).

### Drivers it does not import

`postgres` and `mysql` are built in and selected by `driver`. Anything else —
SQLite, a cloud connector, a Unix-socket proxy, `sqlmock` in a test — is passed in
whole:

```go
db, err := database.Open(ctx, cfg, database.WithDialector(
    postgres.New(postgres.Config{Conn: existingPool}),
))
```

That bypasses DSN construction entirely; the pool settings, logging and
instrumentation still apply, which is also how this package's own tests exercise
`Open` without a database to talk to.

## Migrations

`migrate` applies versioned SQL migrations over
[goose](https://github.com/pressly/goose). It takes an open `*sql.DB` and an
`fs.FS`, so the pool stays whoever opened it's problem and the migrations can be
embedded in the binary that applies them:

```go
//go:embed *.sql
var files embed.FS

m, err := migrate.New(pool.SQL(), files, cfg.Migrate, migrate.WithLogger(log))
applied, err := m.Up(ctx)
```

In this repository that is `internal/migrations` and the `db` command:

```
tehran db migrate            # apply every pending migration
tehran db migrate --to 20260801120000
tehran db status             # every migration, applied or pending
tehran db version            # the recorded version; non-zero exit if anything is pending
make migrate-new NAME=create_accounts
```

`db version` exiting non-zero on pending migrations is what makes it usable as a
deploy gate.

### It is a separate invocation, not something the server does on the way up

This is the decision the rest of the module follows from, and there are four
independent reasons for it. The server runs N replicas that start together and
would race the same DDL. A migration failure should fail a deploy step, not
crash-loop a service. DDL needs a role that may `ALTER`, and the serving role
should not have one — separate invocation, separate credential. And rolling an
image back must not roll a schema back with it.

`DB` from `database` is deliberately not a `lifecycle.Component` for a related
reason; `Migrator` is not one either, and has no `Close`. It was handed a pool
rather than opening one, so the pool's owner closes it.

The `db` command opens its own pool of **two** connections, not the server's 25: a
session-level advisory lock is held on its own connection for the whole run, so
the statements need a second, and every connection a migration holds is one the
running service cannot. It also sets up no OTLP pipeline, deliberately — a
migration is the step a deploy is gated on, so it gets the smallest set of things
that can fail before the schema changes.

### Locking is the part that decides whether a retried Job is safe

Two runners starting together is the ordinary case, not the pathological one: a
Job with `backoffLimit` above 0, or two overlapping deploys. Unlocked, both see
the same migration pending, both run its statements, and the loser fails on the
version table's primary key *after* the fact.

| `lock_mode` | How | When |
|---|---|---|
| `session` | PostgreSQL session-level advisory lock, held on one connection, released by the server if the process dies | Against a database. The default |
| `table` | A lease on a row in `<table_name>_lock`, kept alive by a heartbeat | Behind a pooler in transaction mode (PgBouncer, RDS Proxy), where consecutive statements are not guaranteed the same session and an advisory lock taken on one connection is invisible from the next |
| `none` | Nothing | The only option on MySQL, and an explicit choice rather than a default |

**goose implements a locker for PostgreSQL only.** So `session` or `table` on any
other dialect is *refused at startup* rather than quietly downgraded: a service
that has to run unlocked should say `lock_mode = "none"` in its configuration,
because two runners applying the same migration is not something the next person
reading that configuration can infer from a setting's absence.
`TestMigrateLockModeSuitsTheDriver` in `internal/config` fails if the shipped file
pairs a non-Postgres driver with a locking mode, which would otherwise surface
during a deploy and nowhere earlier.

`lock_wait` has to cover the *other* runner's whole migration, not just its
startup — the loser waiting five minutes is the correct outcome. goose retries on
a fixed five-second period, so the value is rounded up to a multiple of it.
`lock_id` matters when two services migrate two schemas in one PostgreSQL
database: the advisory-lock namespace is per database, not per schema, so they
serialise against each other until they are given different IDs.

**Reads never take the lock, and that took work.** goose enables locking per
*provider* and then takes the lock inside `Status` and `GetDBVersion`, so a
`Migrator` built the obvious way makes `db status` wait out the whole of
`lock_wait` while a migration is running — which is exactly when somebody runs it.
Whether to lock is a property of the operation, not of the provider, so a
`Migrator` holds two: the writer with locking as configured, and a lock-free
reader behind `Status`, `Version`, `HasPending` and `Sources`. The trade is that a
read can catch a run half way, with a version applied a moment ago still showing
as pending — for a diagnostic, far better than a five-minute wait.
`TestEndToEndReadsDoNotQueueBehindARunningMigration` fails if the two are ever
collapsed back into one, and `TestEndToEndSessionLockSerialisesConcurrentRunners`
fails if the split loses the writer its lock.

### A half-finished run reports what it applied

`Up` returns the migrations that landed **alongside** the error, not instead of
it. goose reports them only through `*goose.PartialError`, with a nil result slice
beside it, so a wrapper returning `results, err` verbatim would describe a
half-finished run as having applied nothing — the opposite of true, and the first
thing anybody diagnosing one looks at.
`TestPartialFailureReportsWhatWasApplied` covers it, and
`TestEndToEndPartialFailureLeavesPredecessorsApplied` covers the same thing
against a database that really does reject the statement.

Each migration runs in its own transaction, so a failure rolls back its own
migration and leaves its predecessors applied. Two consequences worth knowing:

- `SET LOCAL lock_timeout = '3s'` as the first statement of an `Up` block is how a
  DDL statement is kept from queueing behind a long read and blocking every write
  that arrives after it. Without it, an `ALTER TABLE` waiting for a lock is
  indistinguishable from an outage.
- `CREATE INDEX CONCURRENTLY` cannot run in a transaction and needs goose's
  no-transaction annotation, which trades atomicity away: a build that fails half
  way leaves an invalid index to be dropped and rebuilt rather than retried.

`allow_out_of_order` is off. Two branches merging with interleaved versions is
exactly the case where applying quietly leaves two environments on the same
version number with a different schema. `timeout` is off too, and is the one
duration in these `Config`s where 0 does not mean "use the default": a bound low
enough to catch a hung migration also cuts a legitimate concurrent index build
short, and the deadline on the job that runs this is the better backstop.

### Expand and contract, or a rollout breaks itself

A migration that drops or narrows a column in the same release as the code change
breaks every replica still running the previous image, which is all of them for
the length of the rollout. Three releases, in order:

1. Add the nullable column or the new table. Old code ignores it.
2. Write both, read the new one.
3. Drop the old column.

Nothing here enforces that. `atlas migrate lint` over the migrations directory
catches the class — destructive change, backwards-incompatible change,
table-locking change — in CI, without Atlas touching production or replacing
anything in this module. Check its current directory-format flag against Atlas's
own documentation; the spelling has moved between releases.

### What it does not do

No schema diffing and no declarative desired-state: the migrations are the source
of truth, hand-written and reviewed as SQL. Nothing generates them from the GORM
models, which means a model and the schema can disagree — a real cost, paid to
keep the DDL something a reviewer reads before it runs.

Note also that goose logs each statement it executes, migrations being reviewed
code rather than user input. That is the opposite of `database`'s
`include_query_values` stance, and it is worth remembering before writing a data
migration with literal values in it.

Versions are timestamps, not a counter. Two branches each adding "the next"
sequential number merge cleanly and then collide at run time, where the collision
is nobody's review comment.

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

There are two per-request lines, and which one you get says where the request
died:

| Line | Means |
|---|---|
| `rpc` | it reached a handler; carries `code`, `duration`, `trace_id` |
| `rpc rejected` | it never became an RPC; carries `procedure`, `peer`, `duration`, `proto` |

`rpc rejected` exists because no interceptor can see that class of failure.
Connect decodes the request message *before* the interceptor chain runs, so a
body cut off by `read_timeout`, an undecodable message, or an unroutable path
reaches neither the logging nor the tracing interceptor — a slow-body client
would pin streams while leaving no trace at all. A middleware around the mux
reports anything that no interceptor accounted for.

It deliberately carries no status code. Connect requires the `ResponseWriter` to
implement `http.Flusher` and checks with a **direct type assertion**, not through
`http.ResponseController`, so a wrapper that captured the status without
reimplementing `Flush` would break every streaming RPC with `CodeInternal`. Path,
peer and duration are enough to see the traffic.

gRPC health and reflection are exempt. They are mounted without the handler
options that carry the interceptors, so nothing logs them — deliberately, since a
health check runs every few seconds — which means they would otherwise be
reported as rejected every single time.
`TestHealthAndReflectionAreNotReportedAsRejected` fails if that exemption breaks,
and `TestServedRPCIsNotDoubleLogged` fails if a real RPC starts getting both
lines.

The `code` on an `rpc` line is classified the way Connect's protocol layer
eventually will, not by `connect.CodeOf` alone. `CodeOf` reports `unknown` for a
bare `context.Canceled` or `context.DeadlineExceeded`: Connect *does* map those
for the response, but in a protocol layer that runs after the whole interceptor
chain has returned, through an unexported helper. Using it directly would log
every client disconnect and every timeout as `unknown` while the caller
correctly received `canceled` or `deadline_exceeded`.

Correlation is read from the context, so it only works if you pass one. Use
`log.InfoContext(ctx, ...)` or `log.LogAttrs(ctx, ...)`; plain `log.Info(...)`
produces a line with no `trace_id`. For the same reason the otel interceptor is
registered ahead of the logging one inside `connectrpc.New` — Connect makes the
first interceptor outermost, so the reverse order would leave every RPC log line
uncorrelated. `TestRPCLogLineSeesSpanContext` exists to keep that from
regressing.
