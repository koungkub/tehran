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
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/koungkub/tehran/internal/config"
	"github.com/koungkub/tehran/internal/greeter"
	"github.com/koungkub/tehran/internal/version"
	"github.com/koungkub/tehran/pkg/connectrpc"
	"github.com/koungkub/tehran/pkg/lifecycle"
	"github.com/koungkub/tehran/pkg/ops"
	"github.com/koungkub/tehran/pkg/telemetry"
)

const telemetryFlushTimeout = 5 * time.Second

// errDraining is what the readiness check reports once shutdown has begun. It
// is a state, not a failure, so it never reaches an error return.
var errDraining = errors.New("draining")

// App is the wired-up api command: the supervisor that runs the servers, the
// telemetry pipeline, and the readiness flag.
type App struct {
	log   *slog.Logger
	tel   *telemetry.Telemetry
	sup   *lifecycle.Supervisor
	ready atomic.Bool
}

// New wires the application: telemetry, then the domain services, then the
// transports in front of them. Nothing is started here — see Run.
func New(ctx context.Context, cfg *config.Config) (*App, error) {
	// The build stamp is linked into this binary, so the telemetry library
	// cannot reach it; hand it over instead.
	cfg.Otel.ServiceVersion = version.Version

	log, err := telemetry.NewLogger(cfg.Log)
	if err != nil {
		return nil, err
	}
	tel, err := telemetry.Setup(ctx, cfg.Otel)
	if err != nil {
		return nil, err
	}

	a := &App{log: log, tel: tel}

	// 1. Outbound adapters — repositories (persistence) and network clients.
	//    Construct them here and inject into the business logic below. greeter
	//    needs neither yet; a domain that does would wire, e.g.:
	//        accountRepo := postgres.NewAccountRepository(db)
	//        payClient    := paygate.NewClient(cfg.Paygate)

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

	opsServer := ops.New(cfg.Ops,
		ops.WithLogger(log),
		ops.WithRegistry(tel.PromRegistry),
		ops.WithReadyCheck("drain", func(context.Context) error {
			if !a.ready.Load() {
				return errDraining
			}
			return nil
		}),
	)

	// 4. Lifecycle. Registration order is start order and shutdown runs in
	//    reverse, so ops comes first: it is up before the RPC server and keeps
	//    answering /readyz and /metrics for the whole time the RPC server is
	//    draining. Reversing these two would close the probe port first and
	//    leave an orchestrator blind during the drain.
	a.sup = lifecycle.New(cfg.Lifecycle,
		lifecycle.WithLogger(log),
		lifecycle.WithComponents(opsServer, rpcServer),
		// Every port is still open when this runs, so a load balancer sees the
		// instance leave service before anything stops accepting connections.
		lifecycle.BeforeShutdown(func() { a.ready.Store(false) }),
	)
	return a, nil
}

// Run starts the components and blocks until SIGINT/SIGTERM or a component
// failure, then drains them in reverse order and flushes telemetry.
func (a *App) Run(ctx context.Context) error {
	a.ready.Store(true)
	err := a.sup.Run(ctx)

	// After the supervisor returns: everything has stopped, so nothing is still
	// producing spans or metrics to flush.
	flushCtx, cancel := context.WithTimeout(context.Background(), telemetryFlushTimeout)
	defer cancel()
	return errors.Join(err, a.tel.Shutdown(flushCtx))
}
