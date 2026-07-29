// Package api is the composition root for the `api` command. It imports the
// shared logic (business logic, repositories, network clients) and wires it
// behind the ConnectRPC server, then owns the run/shutdown lifecycle.
//
// Sibling command apps (e.g. internal/app/consumer) wire the SAME shared
// packages behind a different inbound transport (Kafka). Nothing here is
// reused by cloning code — only by importing the shared packages.
package api

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/koungkub/tehran/internal/config"
	"github.com/koungkub/tehran/internal/greeter"
	"github.com/koungkub/tehran/internal/version"
	"github.com/koungkub/tehran/pkg/connectrpc"
	"github.com/koungkub/tehran/pkg/database"
	"github.com/koungkub/tehran/pkg/lifecycle"
	"github.com/koungkub/tehran/pkg/ops"
	"github.com/koungkub/tehran/pkg/telemetry"
)

const telemetryFlushTimeout = 5 * time.Second

// errDraining is what the readiness check reports once shutdown has begun. It
// is a state, not a failure, so it never reaches an error return.
var errDraining = errors.New("draining")

// App is the wired-up api command: the supervisor that runs the servers, the
// telemetry pipeline, the database pool, and the readiness flag.
type App struct {
	log *zerolog.Logger
	tel *telemetry.Telemetry
	// db is nil when the database section is disabled, which is what lets this
	// command run with no database at all.
	db    *database.DB
	sup   *lifecycle.Supervisor
	ready atomic.Bool
}

// New wires the application: telemetry, then the domain services, then the
// transports in front of them. Nothing is started here — see Run.
//
// The error is named so that anything opened here can be released when a later
// step fails: New's caller gets an error and no handle to close things with.
func New(ctx context.Context, cfg *config.Config) (_ *App, err error) {
	// The build stamp is linked into this binary, so the telemetry library
	// cannot reach it; hand it over instead.
	cfg.Otel.ServiceVersion = version.Version

	log, err := telemetry.NewLogger(cfg.Log)
	if err != nil {
		return nil, err
	}
	// The logger goes in so that a failed span export is reported on the
	// service's own stream at a level something can filter on, rather than
	// through the standard library's log package with no level at all.
	tel, err := telemetry.Setup(ctx, cfg.Otel, telemetry.WithErrorLogger(log))
	if err != nil {
		return nil, err
	}

	app := &App{
		log: log,
		tel: tel,
	}

	// 1. Outbound adapters — repositories (persistence) and network clients.
	//    Construct them here and inject into the business logic below. greeter
	//    needs neither yet; a domain that does would wire, e.g.:
	//        accountRepo := postgres.NewAccountRepository(app.db.Gorm())
	//        payClient    := paygate.NewClient(cfg.Paygate)
	//
	//    One pool is shared by every repository: it is the service's budget of
	//    connections to that server, and a pool per domain would multiply it by
	//    the number of domains.
	if cfg.Database.Enabled {
		db, openErr := database.Open(ctx, cfg.Database.Config,
			database.WithLogger(log),
			// The provider interfaces again, not the Telemetry struct.
			database.WithTracerProvider(tel.TracerProvider),
			database.WithMeterProvider(tel.MeterProvider),
		)
		if openErr != nil {
			return nil, openErr
		}
		app.db = db
		// The pool is open from here on, and only Run closes it. Anything below
		// that fails would otherwise leave it open with nothing left holding a
		// reference to it.
		defer func() {
			if err != nil {
				err = errors.Join(err, app.db.Close())
			}
		}()
	}

	// 2. Business logic — shared domain services. Inject the repos/clients
	//    from step 1 through the constructor as each domain grows.
	greeterSvc := greeter.NewService(log)

	// 3. Inbound transport — ConnectRPC handlers. Each is a connectrpc.Module
	//    and registers itself, so the server never names an individual domain.
	greeterAPI := greeter.NewHandler(greeterSvc)

	rpcServer, err := connectrpc.New(cfg.Server,
		connectrpc.WithModules(greeterAPI),
		connectrpc.WithLogger(log),
		// The provider interfaces, not the Telemetry struct: that is what keeps
		// pkg/connectrpc independent of pkg/telemetry.
		connectrpc.WithTracerProvider(tel.TracerProvider),
		connectrpc.WithMeterProvider(tel.MeterProvider),
	)
	if err != nil {
		return nil, err
	}

	opsOpts := []ops.Option{
		ops.WithLogger(log),
		ops.WithRegistry(tel.PromRegistry),
		ops.WithReadyCheck("drain", func(context.Context) error {
			if !app.ready.Load() {
				return errDraining
			}
			return nil
		}),
	}
	if app.db != nil {
		// Readiness, not liveness: an instance that cannot reach the database
		// cannot serve, but restarting it will not bring the database back.
		opsOpts = append(opsOpts, ops.WithReadyCheck(app.db.Name(), app.db.Ping))
	}
	opsServer := ops.New(cfg.Ops, opsOpts...)

	// 4. Lifecycle. Registration order is start order and shutdown runs in
	//    reverse, so ops comes first: it is up before the RPC server and keeps
	//    answering /readyz and /metrics for the whole time the RPC server is
	//    draining. Reversing these two would close the probe port first and
	//    leave an orchestrator blind during the drain.
	app.sup = lifecycle.New(cfg.Lifecycle,
		lifecycle.WithLogger(log),
		lifecycle.WithComponents(opsServer, rpcServer),
		// Every port is still open when this runs, so a load balancer sees the
		// instance leave service before anything stops accepting connections.
		//
		// Both signals, because they answer different clients: an orchestrator
		// polls /readyz over HTTP, while a gRPC-native balancer watches
		// grpc.health.v1.Health and would otherwise be told SERVING for the whole
		// drain.
		lifecycle.BeforeShutdown(func() {
			app.ready.Store(false)
			rpcServer.SetServing(false)
		}),
	)
	return app, nil
}

// Run starts the components and blocks until SIGINT/SIGTERM or a component
// failure, then drains them in reverse order, closes the database and flushes
// telemetry.
func (a *App) Run(ctx context.Context) error {
	a.ready.Store(true)
	err := a.sup.Run(ctx)

	// The database closes after the supervisor returns rather than as one of its
	// components: registered as a component it would be shut down partway through
	// the sequence, pulling the pool out from under handlers that are still
	// draining. Here every user of it has already stopped.
	errs := []error{err}
	if a.db != nil {
		errs = append(errs, a.db.Close())
	}

	// Nothing is still producing spans or metrics to flush at this point either.
	flushCtx, cancel := context.WithTimeout(context.Background(), telemetryFlushTimeout)
	defer cancel()
	return errors.Join(append(errs, a.tel.Shutdown(flushCtx))...)
}
