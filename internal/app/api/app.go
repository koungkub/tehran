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
	"fmt"
	"net/http"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/koungkub/tehran/internal/greeter"
	"github.com/koungkub/tehran/internal/platform/config"
	"github.com/koungkub/tehran/internal/platform/server"
	"github.com/koungkub/tehran/internal/platform/telemetry"
)

const telemetryFlushTimeout = 5 * time.Second

type App struct {
	cfg       *config.Config
	log       *zap.Logger
	tel       *telemetry.Telemetry
	rpcServer *http.Server
	opsServer *http.Server
	ready     atomic.Bool
}

func New(ctx context.Context, cfg *config.Config) (*App, error) {
	log, err := telemetry.NewLogger(cfg.Log)
	if err != nil {
		return nil, err
	}
	tel, err := telemetry.Setup(ctx, cfg.Otel)
	if err != nil {
		return nil, err
	}

	a := &App{cfg: cfg, log: log, tel: tel}

	// 1. Outbound adapters — repositories (persistence) and network clients.
	//    Construct them here and inject into the business logic below. greeter
	//    needs neither yet; a domain that does would wire, e.g.:
	//        accountRepo := postgres.NewAccountRepository(db)
	//        payClient    := paygate.NewClient(cfg.Paygate)

	// 2. Business logic — shared domain services. Inject the repos/clients
	//    from step 1 through the constructor as each domain grows.
	greeterSvc := greeter.NewService(log)

	// 3. Inbound transport — ConnectRPC handlers. Each is a server.Module and
	//    registers itself, so server.New never names an individual domain.
	greeterAPI := greeter.NewHandler(greeterSvc)

	rpcServer, err := server.New(cfg.Server, log, tel, greeterAPI)
	if err != nil {
		return nil, err
	}
	a.rpcServer = rpcServer
	a.opsServer = server.NewOps(cfg.Ops, tel.PromRegistry, &a.ready)
	return a, nil
}

// Run starts both servers and blocks until SIGINT/SIGTERM or a server
// failure, then drains in-flight requests and flushes telemetry.
func (a *App) Run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		a.log.Info("rpc server listening", zap.String("addr", a.rpcServer.Addr))
		if err := a.rpcServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("rpc server: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		a.log.Info("ops server listening", zap.String("addr", a.opsServer.Addr))
		if err := a.opsServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("ops server: %w", err)
		}
		return nil
	})
	a.ready.Store(true)

	g.Go(func() error {
		<-gctx.Done()
		a.ready.Store(false)
		a.log.Info("shutting down", zap.Duration("timeout", a.cfg.Server.ShutdownTimeout))
		// Fresh context: the signal context is already cancelled.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), a.cfg.Server.ShutdownTimeout)
		defer cancel()
		return errors.Join(
			a.rpcServer.Shutdown(shutdownCtx),
			a.opsServer.Shutdown(shutdownCtx),
		)
	})

	err := g.Wait()

	flushCtx, cancel := context.WithTimeout(context.Background(), telemetryFlushTimeout)
	defer cancel()
	err = errors.Join(err, a.tel.Shutdown(flushCtx))
	_ = a.log.Sync() // Sync on a terminal returns ENOTTY; deliberately ignored.
	return err
}
