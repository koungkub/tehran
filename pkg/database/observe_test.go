package database

import (
	"context"
	"errors"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/gorm"
)

// widget is a model to hang a table name off, so a span can be asserted to name
// what it operated on.
type widget struct {
	ID   int
	Name string
}

type harness struct {
	db     *DB
	mock   sqlmock.Sqlmock
	spans  *tracetest.SpanRecorder
	reader sdkmetric.Reader
}

// newHarness opens a DB with real SDK providers, so the assertions are about the
// spans and measurements that actually reach a backend rather than about calls
// made against a double.
func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	conn, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("new sqlmock: %v", err)
	}
	mock.ExpectPing()

	spans := tracetest.NewSpanRecorder()
	reader := sdkmetric.NewManualReader()
	db, err := Open(context.Background(), cfg,
		WithLogger(quiet()),
		withMockDialector(conn),
		WithTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))),
		WithMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		mock.ExpectClose()
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return &harness{db: db, mock: mock, spans: spans, reader: reader}
}

func (h *harness) lastSpan(t *testing.T) sdktrace.ReadOnlySpan {
	t.Helper()
	ended := h.spans.Ended()
	if len(ended) == 0 {
		t.Fatal("no span was recorded for the statement")
	}
	return ended[len(ended)-1]
}

func spanAttr(t *testing.T, span sdktrace.ReadOnlySpan, key string) (attribute.Value, bool) {
	t.Helper()
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

// collect returns the metrics one scrape would see, which is also what forces the
// pool's observable instruments to be read.
func (h *harness) collect(t *testing.T) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	return rm
}

func findMetric(rm metricdata.ResourceMetrics, name string) (metricdata.Metrics, bool) {
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == name {
				return m, true
			}
		}
	}
	return metricdata.Metrics{}, false
}

func TestStatementSpanNamesTheOperationAndTable(t *testing.T) {
	h := newHarness(t, Config{Host: "db.internal", Port: 5432, Database: "billing"})
	h.mock.ExpectQuery(`SELECT \* FROM "widgets"`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}).AddRow(1, "bolt"))

	var widgets []widget
	if err := h.db.Gorm().WithContext(context.Background()).Find(&widgets).Error; err != nil {
		t.Fatalf("query: %v", err)
	}

	span := h.lastSpan(t)
	// The convention a tracing backend groups on: the operation and its target.
	// The table is not known when the span starts — GORM resolves it while
	// building the SQL — so a fixed name here would mean every query in the
	// service shared one.
	if span.Name() != "SELECT widgets" {
		t.Errorf("span name = %q, want %q", span.Name(), "SELECT widgets")
	}
	if span.SpanKind() != trace.SpanKindClient {
		t.Errorf("span kind = %v, want client", span.SpanKind())
	}
	for _, tc := range []struct{ key, want string }{
		{"db.system.name", "postgresql"},
		{"db.namespace", "billing"},
		{"db.operation.name", "SELECT"},
		{"db.collection.name", "widgets"},
		{"server.address", "db.internal"},
	} {
		got, ok := spanAttr(t, span, tc.key)
		if !ok {
			t.Errorf("span has no %s attribute", tc.key)
			continue
		}
		if got.AsString() != tc.want {
			t.Errorf("%s = %q, want %q", tc.key, got.AsString(), tc.want)
		}
	}
	if got, ok := spanAttr(t, span, "db.response.returned_rows"); !ok || got.AsInt64() != 1 {
		t.Errorf("db.response.returned_rows = %v (present %v), want 1", got.AsInt64(), ok)
	}
}

// TestSpanKeepsBoundValuesOut is the trace-side half of the redaction default:
// the statement is worth recording, the data it carries is not.
func TestSpanKeepsBoundValuesOut(t *testing.T) {
	h := newHarness(t, Config{Database: "billing"})
	h.mock.ExpectQuery(`SELECT \* FROM "widgets"`).
		WithArgs("bolt").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}))

	var widgets []widget
	if err := h.db.Gorm().WithContext(context.Background()).
		Where("name = ?", "bolt").Find(&widgets).Error; err != nil {
		t.Fatalf("query: %v", err)
	}

	query, ok := spanAttr(t, h.lastSpan(t), "db.query.text")
	if !ok {
		t.Fatal("span has no db.query.text attribute")
	}
	if strings.Contains(query.AsString(), "bolt") {
		t.Errorf("db.query.text = %q, want the placeholder rather than the value", query.AsString())
	}
	if !strings.Contains(query.AsString(), "$1") {
		t.Errorf("db.query.text = %q, want the placeholder left in place", query.AsString())
	}
}

func TestIncludeQueryValuesPutsThemInTheSpan(t *testing.T) {
	h := newHarness(t, Config{Database: "billing", IncludeQueryValues: true})
	h.mock.ExpectQuery(`SELECT \* FROM "widgets"`).
		WithArgs("bolt").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}))

	var widgets []widget
	if err := h.db.Gorm().WithContext(context.Background()).
		Where("name = ?", "bolt").Find(&widgets).Error; err != nil {
		t.Fatalf("query: %v", err)
	}

	query, ok := spanAttr(t, h.lastSpan(t), "db.query.text")
	if !ok {
		t.Fatal("span has no db.query.text attribute")
	}
	if !strings.Contains(query.AsString(), "bolt") {
		t.Errorf("db.query.text = %q, want the bound value when IncludeQueryValues is set", query.AsString())
	}
}

func TestFailedStatementMarksTheSpan(t *testing.T) {
	h := newHarness(t, Config{Database: "billing"})
	h.mock.ExpectQuery(`SELECT \* FROM "widgets"`).WillReturnError(errors.New("relation does not exist"))

	var widgets []widget
	if err := h.db.Gorm().WithContext(context.Background()).Find(&widgets).Error; err == nil {
		t.Fatal("query = nil error, want the driver's")
	}

	span := h.lastSpan(t)
	if span.Status().Code != codes.Error {
		t.Errorf("span status = %v, want error", span.Status().Code)
	}
	if len(span.Events()) == 0 {
		t.Error("span records no exception event for the failure")
	}
}

// TestMissingRowIsNotASpanError is the same judgement the log level makes: First
// on an empty set is a documented result, and a trace that flags it as an error
// makes every empty lookup look like an outage.
func TestMissingRowIsNotASpanError(t *testing.T) {
	h := newHarness(t, Config{Database: "billing"})
	h.mock.ExpectQuery(`SELECT \* FROM "widgets"`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}))

	var w widget
	if err := h.db.Gorm().WithContext(context.Background()).First(&w).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("query err = %v, want ErrRecordNotFound", err)
	}

	span := h.lastSpan(t)
	if span.Status().Code == codes.Error {
		t.Error("span status = error for a row that does not exist, want unset")
	}
	if _, ok := spanAttr(t, span, "error.type"); ok {
		t.Error("span carries error.type for a row that does not exist")
	}
}

// TestStatementSpanJoinsTheCallersTrace is what makes any of this useful: the
// statement has to hang under the request that caused it, and the request's own
// span has to survive it. Ending the wrong span would truncate the trace at the
// first query.
func TestStatementSpanJoinsTheCallersTrace(t *testing.T) {
	h := newHarness(t, Config{Database: "billing"})
	h.mock.ExpectQuery(`SELECT \* FROM "widgets"`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}))

	tracer := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(h.spans)).Tracer("test")
	ctx, caller := tracer.Start(context.Background(), "handler")

	var widgets []widget
	if err := h.db.Gorm().WithContext(ctx).Find(&widgets).Error; err != nil {
		t.Fatalf("query: %v", err)
	}

	statement := h.lastSpan(t)
	if statement.Parent().SpanID() != caller.SpanContext().SpanID() {
		t.Errorf("statement span parent = %v, want the caller's span %v",
			statement.Parent().SpanID(), caller.SpanContext().SpanID())
	}
	if !caller.IsRecording() {
		t.Error("the caller's span was ended by the statement's instrumentation")
	}
	caller.End()
}

// TestConnectionAttrsNeverCarryCredentials covers a leak that url.Parse invites:
// it accepts far more than URLs, so a libpq key/value DSN parses with no error at
// all and lands whole — password included — in Path, and a MySQL DSN parses to an
// opaque. Reading the host and database back out of either would publish the
// credentials to a trace backend as db.namespace, where they would also be
// indexed and searchable.
func TestConnectionAttrsNeverCarryCredentials(t *testing.T) {
	const secret = "hunter2"
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{
			name: "libpq key/value dsn",
			cfg:  Config{DSN: "host=db.internal port=5432 user=svc password=" + secret + " dbname=billing"},
		},
		{
			name: "mysql dsn",
			cfg:  Config{Driver: DriverMySQL, DSN: "svc:" + secret + "@tcp(db.internal:3306)/billing?parseTime=true"},
		},
		{
			name: "url dsn",
			cfg:  Config{DSN: "postgres://svc:" + secret + "@db.internal:5432/billing?sslmode=require"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, kv := range connectionAttrs(tc.cfg.withDefaults(), tc.cfg.Driver, DefaultName) {
				if strings.Contains(kv.Value.String(), secret) {
					t.Errorf("%s = %q carries the password", kv.Key, kv.Value.String())
				}
			}
		})
	}

	// The URL case still has to report the server, or the guard above would have
	// been satisfied by reporting nothing at all.
	attrs := connectionAttrs(Config{DSN: "postgres://svc:" + secret + "@db.internal:5432/billing"}.withDefaults(),
		DriverPostgres, DefaultName)
	found := map[string]string{}
	for _, kv := range attrs {
		found[string(kv.Key)] = kv.Value.String()
	}
	if found["db.namespace"] != "billing" || found["server.address"] != "db.internal" || found["server.port"] != "5432" {
		t.Errorf("attributes from a url dsn = %v, want the server and database read out of it", found)
	}
}

// TestSystemNameFollowsTheDialector guards against labelling a connection with
// whatever Config.Driver happened to default to: WithDialector ignores that field
// entirely, so a SQLite or Spanner connection would report itself as postgresql.
func TestSystemNameFollowsTheDialector(t *testing.T) {
	h := newHarness(t, Config{Driver: DriverMySQL, Database: "billing"})
	h.mock.ExpectQuery(`SELECT \* FROM "widgets"`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}))

	var widgets []widget
	if err := h.db.Gorm().WithContext(context.Background()).Find(&widgets).Error; err != nil {
		t.Fatalf("query: %v", err)
	}

	// The harness opens through a postgres dialector while Config says mysql.
	got, ok := spanAttr(t, h.lastSpan(t), "db.system.name")
	if !ok {
		t.Fatal("span has no db.system.name attribute")
	}
	if got.AsString() != "postgresql" {
		t.Errorf("db.system.name = %q, want postgresql from the dialector, not mysql from Config", got.AsString())
	}
}

// TestStatementSpansStaySiblings pins the shape of a trace across several
// statements. The before-callback replaces the statement's context with one
// carrying its span, so if GORM ever reused a statement rather than cloning it
// per call, each query would parent to the previous query's ended span and a
// transaction would render as a ladder instead of a fan.
func TestStatementSpansStaySiblings(t *testing.T) {
	h := newHarness(t, Config{Database: "billing"})
	h.mock.ExpectBegin()
	h.mock.ExpectQuery(`SELECT \* FROM "widgets"`).WillReturnRows(sqlmock.NewRows([]string{"id", "name"}))
	h.mock.ExpectQuery(`SELECT \* FROM "widgets"`).WillReturnRows(sqlmock.NewRows([]string{"id", "name"}))
	h.mock.ExpectCommit()

	tracer := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(h.spans)).Tracer("test")
	ctx, caller := tracer.Start(context.Background(), "handler")
	defer caller.End()

	err := h.db.Gorm().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var widgets []widget
		if err := tx.Find(&widgets).Error; err != nil {
			return err
		}
		return tx.Find(&widgets).Error
	})
	if err != nil {
		t.Fatalf("transaction: %v", err)
	}

	ended := h.spans.Ended()
	if len(ended) != 2 {
		t.Fatalf("spans = %d, want one per statement", len(ended))
	}
	for _, span := range ended {
		if span.Parent().SpanID() != caller.SpanContext().SpanID() {
			t.Errorf("span %q parent = %v, want the caller's span %v",
				span.Name(), span.Parent().SpanID(), caller.SpanContext().SpanID())
		}
	}
}

func TestStatementDurationIsRecorded(t *testing.T) {
	h := newHarness(t, Config{Database: "billing"})
	h.mock.ExpectQuery(`SELECT \* FROM "widgets"`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}))

	var widgets []widget
	if err := h.db.Gorm().WithContext(context.Background()).Find(&widgets).Error; err != nil {
		t.Fatalf("query: %v", err)
	}

	m, ok := findMetric(h.collect(t), "db.client.operation.duration")
	if !ok {
		t.Fatal("no db.client.operation.duration metric was recorded")
	}
	hist, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("db.client.operation.duration is %T, want a float64 histogram", m.Data)
	}
	if len(hist.DataPoints) != 1 || hist.DataPoints[0].Count != 1 {
		t.Fatalf("data points = %+v, want one measurement", hist.DataPoints)
	}
	if op, ok := hist.DataPoints[0].Attributes.Value("db.operation.name"); !ok || op.AsString() != "SELECT" {
		t.Errorf("db.operation.name = %v, want SELECT", op.AsString())
	}
}

func TestPoolMetricsReportTheStats(t *testing.T) {
	h := newHarness(t, Config{Database: "billing", MaxOpenConns: 7, MaxIdleConns: 3})

	rm := h.collect(t)
	for _, name := range []string{
		"db.client.connection.count",
		"db.client.connection.max",
		"db.client.connection.idle.max",
		"db.client.connection.wait.count",
		"db.client.connection.wait.duration",
		"db.client.connection.closed",
	} {
		if _, ok := findMetric(rm, name); !ok {
			t.Errorf("no %s metric was collected", name)
		}
	}

	m, _ := findMetric(rm, "db.client.connection.max")
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("db.client.connection.max is %T, want an int64 sum", m.Data)
	}
	if len(sum.DataPoints) != 1 || sum.DataPoints[0].Value != 7 {
		t.Errorf("db.client.connection.max = %+v, want 7", sum.DataPoints)
	}

	// The pool name is what tells two pools in one process apart.
	if pool, ok := sum.DataPoints[0].Attributes.Value("db.client.connection.pool.name"); !ok || pool.AsString() != DefaultName {
		t.Errorf("db.client.connection.pool.name = %v, want %q", pool.AsString(), DefaultName)
	}

	count, _ := findMetric(rm, "db.client.connection.count")
	states := count.Data.(metricdata.Sum[int64])
	if len(states.DataPoints) != 2 {
		t.Errorf("db.client.connection.count data points = %d, want one per state (used, idle)", len(states.DataPoints))
	}
}

// TestCloseStopsPoolMetrics is why Close unregisters the callback: it holds the
// pool, so left registered it would keep reading a closed one on every scrape for
// as long as the meter provider lives.
func TestCloseStopsPoolMetrics(t *testing.T) {
	conn, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("new sqlmock: %v", err)
	}
	mock.ExpectPing()
	mock.ExpectClose()

	reader := sdkmetric.NewManualReader()
	db, err := Open(context.Background(), Config{Database: "billing"},
		WithLogger(quiet()),
		withMockDialector(conn),
		WithMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if _, ok := findMetric(rm, "db.client.connection.count"); ok {
		t.Error("the pool callback is still registered after Close")
	}
}

func TestOperationOf(t *testing.T) {
	for _, tc := range []struct {
		sql, fallback, want string
	}{
		{sql: "SELECT * FROM widgets", fallback: "SELECT", want: "SELECT"},
		// Raw and Row run whatever the caller wrote, so their operation can only
		// be read off the statement.
		{sql: "insert into widgets values (1)", fallback: "", want: "INSERT"},
		{sql: "\n  WITH recent AS (SELECT 1) SELECT * FROM recent", fallback: "", want: "WITH"},
		// A leading parenthesis is punctuation, not part of the operation, and
		// keeping it would split one operation across two metric labels.
		{sql: "(SELECT 1)", fallback: "", want: "SELECT"},
		{sql: "", fallback: "UPDATE", want: "UPDATE"},
		{sql: "", fallback: "", want: "SQL"},
	} {
		if got := operationOf(tc.sql, tc.fallback); got != tc.want {
			t.Errorf("operationOf(%q, %q) = %q, want %q", tc.sql, tc.fallback, got, tc.want)
		}
	}
}

func TestErrorTypeExcludesTheCallersOwnOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{name: "no error", err: nil, want: ""},
		{name: "no row", err: gorm.ErrRecordNotFound, want: ""},
		{name: "cancelled", err: context.Canceled, want: "canceled"},
		{name: "timed out", err: context.DeadlineExceeded, want: "deadline_exceeded"},
		{name: "a real failure", err: errors.New("syntax error"), want: "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := errorType(tc.err); got != tc.want {
				t.Errorf("errorType(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestSystemNameKeepsAnUnknownDriverSearchable(t *testing.T) {
	for _, tc := range []struct{ driver, want string }{
		{DriverPostgres, "postgresql"},
		{DriverMySQL, "mysql"},
		{"", "other_sql"},
		{"clickhouse", "clickhouse"},
	} {
		if got := systemName(tc.driver); got != tc.want {
			t.Errorf("systemName(%q) = %q, want %q", tc.driver, got, tc.want)
		}
	}
}
