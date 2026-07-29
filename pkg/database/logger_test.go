package database

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// recorder captures each record as a decoded map, which is the only way to assert
// on a level and a field at once.
type recorder struct {
	mu      sync.Mutex
	records []map[string]any
}

func (r *recorder) Write(p []byte) (int, error) {
	var rec map[string]any
	if err := json.Unmarshal(p, &rec); err != nil {
		return len(p), nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, rec)
	return len(p), nil
}

func (r *recorder) logger(level zerolog.Level) *zerolog.Logger {
	l := zerolog.New(r).Level(level)
	return &l
}

func (r *recorder) last(t *testing.T) map[string]any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.records) == 0 {
		t.Fatal("no record was emitted")
	}
	return r.records[len(r.records)-1]
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.records)
}

func has(rec map[string]any, key string) bool {
	_, ok := rec[key]
	return ok
}

func str(rec map[string]any, key string) string {
	s, _ := rec[key].(string)
	return s
}

// statement is what GORM hands Trace: building the SQL and counting the rows is
// deferred, because doing it for a line that will not be logged is wasted work.
// called records whether that deferred work happened.
func statement(sql string, called *bool) func() (string, int64) {
	return func() (string, int64) {
		if called != nil {
			*called = true
		}
		return sql, 1
	}
}

// TestStatementLevels is the classification this adapter exists for: two of these
// five outcomes are not failures, and logging them at error level is what teaches
// an on-call rotation to ignore database errors.
func TestStatementLevels(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		elapsed time.Duration
		want    zerolog.Level
		msg     string
	}{
		{name: "ordinary statement", want: zerolog.DebugLevel, msg: "sql"},
		{name: "no row found", err: gorm.ErrRecordNotFound, want: zerolog.DebugLevel, msg: "sql not found"},
		{name: "caller gave up", err: context.Canceled, want: zerolog.WarnLevel, msg: "sql aborted"},
		{name: "caller ran out of time", err: context.DeadlineExceeded, want: zerolog.WarnLevel, msg: "sql aborted"},
		{name: "slow statement", elapsed: time.Second, want: zerolog.WarnLevel, msg: "sql slow"},
		{name: "failed statement", err: errors.New("syntax error"), want: zerolog.ErrorLevel, msg: "sql failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			log := newLogger(rec.logger(zerolog.DebugLevel),
				Config{SlowQueryThreshold: 100 * time.Millisecond})

			log.Trace(context.Background(), time.Now().Add(-tc.elapsed),
				statement("SELECT * FROM widgets", nil), tc.err)

			got := rec.last(t)
			if str(got, "level") != tc.want.String() {
				t.Errorf("level = %v, want %v", got["level"], tc.want)
			}
			if str(got, "message") != tc.msg {
				t.Errorf("message = %q, want %q", got["message"], tc.msg)
			}
			if !has(got, "statement") {
				t.Error("record has no statement field")
			}
			if !has(got, "duration") {
				t.Error("record has no duration field")
			}
			if ok := has(got, "error"); ok != (tc.err != nil) {
				t.Errorf("error field present = %v, want %v", ok, tc.err != nil)
			}
		})
	}
}

// A slow statement is a warning at any log level, which is the point of the
// threshold: it is the one statement worth seeing without turning on debug.
func TestSlowStatementIsLoggedAtInfoLevel(t *testing.T) {
	rec := &recorder{}
	log := newLogger(rec.logger(zerolog.InfoLevel), Config{SlowQueryThreshold: 10 * time.Millisecond})

	log.Trace(context.Background(), time.Now().Add(-time.Second),
		statement("SELECT * FROM widgets", nil), nil)

	if got := rec.last(t); str(got, "message") != "sql slow" {
		t.Errorf("message = %q, want %q", got["message"], "sql slow")
	}
}

// TestStatementTextIsNotBuiltWhenItWouldNotBeLogged is what makes per-statement
// logging free at a production level: GORM defers the SQL and the row count to a
// closure, and calling it anyway would pay for every line that is then dropped.
func TestStatementTextIsNotBuiltWhenItWouldNotBeLogged(t *testing.T) {
	rec := &recorder{}
	log := newLogger(rec.logger(zerolog.InfoLevel), Config{SlowQueryThreshold: time.Hour})

	var built bool
	log.Trace(context.Background(), time.Now(), statement("SELECT 1", &built), nil)

	if built {
		t.Error("the statement was built for a record that was never emitted")
	}
	if n := rec.count(); n != 0 {
		t.Errorf("records = %d, want 0 at info level", n)
	}
}

// db.Debug() asks for one statement to be logged, and it has to arrive at a level
// that is actually enabled — otherwise debugging a single query means raising the
// log level of the whole process.
func TestDebugModeLogsAtInfo(t *testing.T) {
	rec := &recorder{}
	log := newLogger(rec.logger(zerolog.InfoLevel), Config{}).LogMode(gormlogger.Info)

	log.Trace(context.Background(), time.Now(), statement("SELECT 1", nil), nil)

	if got := rec.last(t); str(got, "level") != zerolog.InfoLevel.String() {
		t.Errorf("level = %v, want %v", got["level"], zerolog.InfoLevel)
	}
}

// A statement marked for debugging has to be visible even when its outcome is one
// of the quiet ones: "the row is not there" is frequently the thing being
// debugged, and leaving it at debug level would mean db.Debug() printed nothing.
func TestDebugModeShowsAQuietOutcome(t *testing.T) {
	rec := &recorder{}
	log := newLogger(rec.logger(zerolog.InfoLevel), Config{}).LogMode(gormlogger.Info)

	log.Trace(context.Background(), time.Now(), statement("SELECT 1", nil), gorm.ErrRecordNotFound)

	got := rec.last(t)
	if str(got, "level") != zerolog.InfoLevel.String() {
		t.Errorf("level = %v, want %v", got["level"], zerolog.InfoLevel)
	}
	if str(got, "message") != "sql not found" {
		t.Errorf("message = %q, want the outcome still classified as not found", got["message"])
	}
}

func TestSilentModeLogsNothing(t *testing.T) {
	rec := &recorder{}
	log := newLogger(rec.logger(zerolog.DebugLevel), Config{}).LogMode(gormlogger.Silent)

	log.Trace(context.Background(), time.Now(), statement("SELECT 1", nil), errors.New("boom"))
	log.Error(context.Background(), "boom")

	if n := rec.count(); n != 0 {
		t.Errorf("records = %d, want 0 in silent mode", n)
	}
}

// TestGormsOwnMessagesAreRendered covers the shape GORM's non-statement logging
// takes: a printf format string and its arguments, with a trailing newline meant
// for a line-oriented writer.
func TestGormsOwnMessagesAreRendered(t *testing.T) {
	rec := &recorder{}
	log := newLogger(rec.logger(zerolog.DebugLevel), Config{})

	log.Warn(context.Background(), "removing callback `%s` from %s\n", "gorm:query", "repo.go:12")

	got := rec.last(t)
	if str(got, "message") != "removing callback `gorm:query` from repo.go:12" {
		t.Errorf("message = %q, want the arguments substituted and the newline dropped", got["message"])
	}
	if has(got, "data") {
		t.Error("record carries a data field, want the arguments in the message instead")
	}
}

// TestParamsFilterKeepsValuesOutOfLogs covers the default that decides whether
// the rows themselves end up in a log aggregator.
func TestParamsFilterKeepsValuesOutOfLogs(t *testing.T) {
	const sql = "SELECT * FROM users WHERE email = $1"

	rec := &recorder{}
	filter, ok := newLogger(rec.logger(zerolog.DebugLevel), Config{}).(gorm.ParamsFilter)
	if !ok {
		t.Fatal("the logger does not implement gorm.ParamsFilter, so GORM will log bound values")
	}
	if _, params := filter.ParamsFilter(context.Background(), sql, "a@example.com"); params != nil {
		t.Errorf("params = %v, want none: values must not reach a log line by default", params)
	}

	filter = newLogger(rec.logger(zerolog.DebugLevel),
		Config{IncludeQueryValues: true}).(gorm.ParamsFilter)
	if _, params := filter.ParamsFilter(context.Background(), sql, "a@example.com"); len(params) != 1 {
		t.Errorf("params = %v, want the values when IncludeQueryValues is set", params)
	}
}

// TestStatementCallerNamesThisAdapter pins a known limitation rather than a
// feature, so that nobody reads a statement's caller as the repository that ran
// the query.
//
// zerolog resolves a call site from a fixed stack depth, which is right for code
// that calls the logger directly. GORM calls this adapter from inside its own
// callback chain, so the frame at that depth is this file — the same value for
// every statement in the service, whichever repository issued it.
//
// Fixing it means resolving the frame here and handing it to the logger, which was
// tried and removed: it needs a stack search on every emitted statement, costing
// more than everything else on the line, plus a package for the handoff. Since
// log.caller is off by default and the statement lines are at debug, the trade was
// not worth it. If the value is ever needed, note that a repository already
// identifies itself through the statement text.
func TestStatementCallerNamesThisAdapter(t *testing.T) {
	rec := &recorder{}
	conn, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("new sqlmock: %v", err)
	}
	mock.ExpectPing()
	mock.ExpectQuery("SELECT 1").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))

	// Caller resolution is zerolog's, so it has to be switched on the way
	// telemetry.NewLogger switches it.
	base := zerolog.New(rec).Level(zerolog.DebugLevel).With().Caller().Logger()
	db, err := Open(context.Background(), Config{},
		WithLogger(&base), withMockDialector(conn))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		mock.ExpectClose()
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	var n int
	if err := db.Gorm().WithContext(context.Background()).Raw("SELECT 1").Scan(&n).Error; err != nil {
		t.Fatalf("query: %v", err)
	}

	var caller string
	rec.mu.Lock()
	for _, r := range rec.records {
		if str(r, "message") == "sql" {
			caller = str(r, "caller")
		}
	}
	rec.mu.Unlock()
	if caller == "" {
		t.Fatal("the statement record carries no call site")
	}
	if !strings.Contains(caller, "database/logger.go:") {
		t.Errorf("caller = %s, want this adapter's own frame; if this now names the "+
			"calling repository, the limitation is fixed and the docs should say so", caller)
	}
}
