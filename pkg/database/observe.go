package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/gorm"
)

// instrumentationName identifies this package as the source of its spans and
// metrics, which is how a backend attributes them to a library version.
const instrumentationName = "github.com/koungkub/tehran/pkg/database"

// stateKey carries the span and start time of the statement being run, so the
// after-callback ends the span this package started and no other. Reading the
// span back out of the context with trace.SpanFromContext would return the
// caller's own span when the before-callback did not run, and ending that would
// truncate the request's trace.
type stateKey struct{}

type spanState struct {
	span  trace.Span
	start time.Time
}

// instrumentation records a span and a duration measurement per statement.
type instrumentation struct {
	tracer        trace.Tracer
	duration      metric.Float64Histogram
	attrs         []attribute.KeyValue
	includeValues bool
}

// callbackRegistrar is what GORM's per-operation callback groups have in common,
// so the hooks below can be listed as data rather than as twelve calls.
type callbackRegistrar interface {
	Register(name string, fn func(*gorm.DB)) error
}

// registerInstrumentation hooks the statement callbacks. GORM runs the same
// callback chain for every statement it issues, so this covers the queries a
// repository writes and the ones GORM generates for associations alike.
//
// driver is the dialector's own name rather than Config.Driver, so a connection
// opened through WithDialector does not report itself as whatever Config happened
// to default to.
func registerInstrumentation(db *gorm.DB, cfg Config, driver string, o options) error {
	duration, err := o.meterProvider.Meter(instrumentationName).Float64Histogram(
		"db.client.operation.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Duration of database client operations."),
	)
	if err != nil {
		return fmt.Errorf("create duration histogram: %w", err)
	}

	i := &instrumentation{
		tracer:        o.tracerProvider.Tracer(instrumentationName),
		duration:      duration,
		attrs:         connectionAttrs(cfg, driver, o.name),
		includeValues: cfg.IncludeQueryValues,
	}

	// The operation names here are fallbacks. Create, Query, Update and Delete
	// know what they are before any SQL exists; Row and Raw carry whatever the
	// caller wrote, so their operation is read back off the statement.
	callbacks := db.Callback()
	hooks := []struct {
		register callbackRegistrar
		hook     func(*gorm.DB)
		name     string
	}{
		{callbacks.Create().Before("gorm:create"), i.before("INSERT"), "before_create"},
		{callbacks.Create().After("gorm:create"), i.after("INSERT"), "after_create"},
		{callbacks.Query().Before("gorm:query"), i.before("SELECT"), "before_query"},
		{callbacks.Query().After("gorm:query"), i.after("SELECT"), "after_query"},
		{callbacks.Update().Before("gorm:update"), i.before("UPDATE"), "before_update"},
		{callbacks.Update().After("gorm:update"), i.after("UPDATE"), "after_update"},
		{callbacks.Delete().Before("gorm:delete"), i.before("DELETE"), "before_delete"},
		{callbacks.Delete().After("gorm:delete"), i.after("DELETE"), "after_delete"},
		{callbacks.Row().Before("gorm:row"), i.before(""), "before_row"},
		{callbacks.Row().After("gorm:row"), i.after(""), "after_row"},
		{callbacks.Raw().Before("gorm:raw"), i.before(""), "before_raw"},
		{callbacks.Raw().After("gorm:raw"), i.after(""), "after_raw"},
	}
	for _, h := range hooks {
		// Deliberately not the "otel:" prefix: that is what gorm.io's own
		// OpenTelemetry plugin registers under, and a service using both would
		// have one silently replace the other rather than run both.
		if err := h.register.Register("observe:"+h.name, h.hook); err != nil {
			return fmt.Errorf("register callback %s: %w", h.name, err)
		}
	}
	return nil
}

// connectionAttrs are the attributes every span and measurement carries. They
// describe the connection, so they are constant for the life of the pool and add
// no cardinality.
func connectionAttrs(cfg Config, driver, pool string) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		semconv.DBSystemNameKey.String(systemName(driver)),
		semconv.DBClientConnectionPoolName(pool),
	}
	host, port, database := cfg.Host, cfg.Port, cfg.Database
	// A DSN given whole is the only description of the server there is, so read
	// the host and database back out of it — and only what dsnServer is willing to
	// vouch for, since the rest of that string is the credentials.
	if cfg.DSN != "" {
		host, port, database = dsnServer(cfg.DSN)
	}
	if database != "" {
		attrs = append(attrs, semconv.DBNamespace(database))
	}
	if host != "" {
		attrs = append(attrs, semconv.ServerAddress(host))
	}
	if port > 0 {
		attrs = append(attrs, semconv.ServerPort(port))
	}
	return attrs
}

// systemName maps a driver name onto the db.system.name value a backend expects.
// An unrecognised driver — one supplied through WithDialector — reports itself
// rather than being flattened into other_sql, which keeps it searchable.
func systemName(driver string) string {
	switch driver {
	case DriverPostgres:
		return "postgresql"
	case DriverMySQL:
		return "mysql"
	case "":
		return "other_sql"
	}
	return driver
}

func (i *instrumentation) before(operation string) func(*gorm.DB) {
	return func(tx *gorm.DB) {
		ctx := tx.Statement.Context
		if ctx == nil {
			ctx = context.Background()
		}
		// The table is not known yet — GORM resolves it while building the SQL,
		// which happens after this callback — so the span starts under the
		// operation alone and is renamed once there is something to name it
		// after.
		name := operation
		if name == "" {
			name = "sql"
		}
		ctx, span := i.tracer.Start(ctx, name,
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(i.attrs...),
		)
		tx.Statement.Context = context.WithValue(ctx, stateKey{}, &spanState{span: span, start: time.Now()})
	}
}

func (i *instrumentation) after(fallback string) func(*gorm.DB) {
	return func(tx *gorm.DB) {
		ctx := tx.Statement.Context
		state, ok := ctx.Value(stateKey{}).(*spanState)
		if !ok {
			return // Not a statement this package started a span for.
		}
		elapsed := time.Since(state.start)
		operation := operationOf(tx.Statement.SQL.String(), fallback)
		table := tx.Statement.Table
		err := tx.Error

		attrs := make([]attribute.KeyValue, 0, len(i.attrs)+3)
		attrs = append(attrs, i.attrs...)
		attrs = append(attrs, semconv.DBOperationName(operation))
		if table != "" {
			attrs = append(attrs, semconv.DBCollectionName(table))
		}
		// A statement that failed for a reason the caller caused — a cancelled
		// request, a row that is not there — is not an error of the database's,
		// and counting it as one makes an error-rate panel lie.
		if errType := errorType(err); errType != "" {
			attrs = append(attrs, semconv.ErrorTypeKey.String(errType))
		}
		i.duration.Record(ctx, elapsed.Seconds(), metric.WithAttributes(attrs...))

		span := state.span
		defer span.End()
		if !span.IsRecording() {
			return
		}
		span.SetName(spanName(operation, table))
		span.SetAttributes(attrs[len(i.attrs):]...) // The connection attributes are already on it.
		if tx.Statement.SQL.Len() > 0 {
			span.SetAttributes(semconv.DBQueryText(i.statement(tx)))
		}
		if rows := tx.Statement.RowsAffected; rows >= 0 {
			if operation == "SELECT" {
				span.SetAttributes(semconv.DBResponseReturnedRows(int(rows)))
			} else {
				span.SetAttributes(attribute.Int64("db.rows_affected", rows))
			}
		}
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
	}
}

// statement is the SQL as it should appear in a span. Without
// IncludeQueryValues the placeholders are left in, which is both what a query
// plan needs and what keeps the rows themselves out of a trace backend.
func (i *instrumentation) statement(tx *gorm.DB) string {
	sql := tx.Statement.SQL.String()
	if !i.includeValues {
		return sql
	}
	return tx.Explain(sql, tx.Statement.Vars...)
}

// spanName follows the convention a tracing backend groups on: the operation and
// what it operated on.
func spanName(operation, table string) string {
	if table == "" {
		return operation
	}
	return operation + " " + table
}

// operationOf reads the operation off the statement, for the callbacks that do
// not know it in advance: Raw and Row run whatever the caller wrote.
func operationOf(sql, fallback string) string {
	sql = strings.TrimLeft(sql, " \t\r\n(")
	if word, _, _ := strings.Cut(sql, " "); word != "" {
		return strings.ToUpper(word)
	}
	if fallback != "" {
		return fallback
	}
	return "SQL"
}

// errorType classifies a failure into the low-cardinality value error.type is
// meant to hold, and reports no error at all for the two outcomes that are not
// failures: a row that does not exist, and a caller that gave up.
func errorType(err error) string {
	switch {
	case err == nil, errors.Is(err, gorm.ErrRecordNotFound):
		return ""
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	}
	return "other"
}

// registerPoolMetrics observes the pool through database/sql's own statistics.
//
// The instruments are observable, so nothing is measured unless a scrape asks
// for it, and every value in a scrape comes from a single Stats call — the
// numbers within one collection are therefore consistent with each other.
//
// db.client.connection.count, .max and .idle.max are the OpenTelemetry semantic
// convention. The rest have no convention to follow and keep to its prefix: they
// are what tells a saturated pool (waits climbing) apart from a churning one
// (closed{reason="idle"} climbing), which is the difference between raising
// MaxOpenConns and raising MaxIdleConns.
func registerPoolMetrics(db *sql.DB, cfg Config, driver string, o options) (func() error, error) {
	meter := o.meterProvider.Meter(instrumentationName)

	count, err := meter.Int64ObservableUpDownCounter("db.client.connection.count",
		metric.WithUnit("{connection}"),
		metric.WithDescription("The number of connections that are currently in the state described by the state attribute."))
	if err != nil {
		return nil, fmt.Errorf("create pool instruments: %w", err)
	}
	maxConns, err := meter.Int64ObservableUpDownCounter("db.client.connection.max",
		metric.WithUnit("{connection}"),
		metric.WithDescription("The maximum number of open connections allowed."))
	if err != nil {
		return nil, fmt.Errorf("create pool instruments: %w", err)
	}
	maxIdle, err := meter.Int64ObservableUpDownCounter("db.client.connection.idle.max",
		metric.WithUnit("{connection}"),
		metric.WithDescription("The maximum number of idle open connections allowed."))
	if err != nil {
		return nil, fmt.Errorf("create pool instruments: %w", err)
	}
	waits, err := meter.Int64ObservableCounter("db.client.connection.wait.count",
		metric.WithUnit("{request}"),
		metric.WithDescription("The number of times a caller had to wait for a connection."))
	if err != nil {
		return nil, fmt.Errorf("create pool instruments: %w", err)
	}
	waitTime, err := meter.Float64ObservableCounter("db.client.connection.wait.duration",
		metric.WithUnit("s"),
		metric.WithDescription("The total time callers spent waiting for a connection."))
	if err != nil {
		return nil, fmt.Errorf("create pool instruments: %w", err)
	}
	closed, err := meter.Int64ObservableCounter("db.client.connection.closed",
		metric.WithUnit("{connection}"),
		metric.WithDescription("The number of connections closed, by the limit that closed them."))
	if err != nil {
		return nil, fmt.Errorf("create pool instruments: %w", err)
	}

	pool := metric.WithAttributes(
		semconv.DBSystemNameKey.String(systemName(driver)),
		semconv.DBClientConnectionPoolName(o.name),
	)
	used := metric.WithAttributes(semconv.DBClientConnectionStateUsed)
	idle := metric.WithAttributes(semconv.DBClientConnectionStateIdle)
	byIdleLimit := metric.WithAttributes(attribute.String("reason", "idle"))
	byIdleTime := metric.WithAttributes(attribute.String("reason", "idle_time"))
	byLifetime := metric.WithAttributes(attribute.String("reason", "lifetime"))

	registration, err := meter.RegisterCallback(
		func(_ context.Context, obs metric.Observer) error {
			s := db.Stats()
			obs.ObserveInt64(count, int64(s.InUse), pool, used)
			obs.ObserveInt64(count, int64(s.Idle), pool, idle)
			obs.ObserveInt64(maxConns, int64(s.MaxOpenConnections), pool)
			obs.ObserveInt64(maxIdle, int64(cfg.MaxIdleConns), pool)
			obs.ObserveInt64(waits, s.WaitCount, pool)
			obs.ObserveFloat64(waitTime, s.WaitDuration.Seconds(), pool)
			obs.ObserveInt64(closed, s.MaxIdleClosed, pool, byIdleLimit)
			obs.ObserveInt64(closed, s.MaxIdleTimeClosed, pool, byIdleTime)
			obs.ObserveInt64(closed, s.MaxLifetimeClosed, pool, byLifetime)
			return nil
		},
		count, maxConns, maxIdle, waits, waitTime, closed,
	)
	if err != nil {
		return nil, fmt.Errorf("register pool callback: %w", err)
	}
	// The callback holds the pool, so it has to be unregistered when the pool
	// closes or a meter provider keeps reading a closed one every scrape.
	return func() error { return registration.Unregister() }, nil
}
