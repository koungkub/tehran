package database

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// recorder captures records instead of formatting them, which is the only way to
// assert on a level, an attribute, and a call site at once.
type recorder struct {
	level   slog.Level
	records []slog.Record
	attrs   []slog.Attr
}

func (r *recorder) Enabled(_ context.Context, level slog.Level) bool { return level >= r.level }

func (r *recorder) Handle(_ context.Context, rec slog.Record) error {
	r.records = append(r.records, rec)
	return nil
}

func (r *recorder) WithAttrs(attrs []slog.Attr) slog.Handler {
	r.attrs = append(r.attrs, attrs...)
	return r
}

func (r *recorder) WithGroup(string) slog.Handler { return r }

func (r *recorder) last(t *testing.T) slog.Record {
	t.Helper()
	if len(r.records) == 0 {
		t.Fatal("no record was emitted")
	}
	return r.records[len(r.records)-1]
}

func hasAttr(t *testing.T, rec slog.Record, key string) bool {
	t.Helper()
	var found bool
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			found = true
			return false
		}
		return true
	})
	return found
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
		want    slog.Level
		msg     string
	}{
		{name: "ordinary statement", want: slog.LevelDebug, msg: "sql"},
		{name: "no row found", err: gorm.ErrRecordNotFound, want: slog.LevelDebug, msg: "sql not found"},
		{name: "caller gave up", err: context.Canceled, want: slog.LevelWarn, msg: "sql aborted"},
		{name: "caller ran out of time", err: context.DeadlineExceeded, want: slog.LevelWarn, msg: "sql aborted"},
		{name: "slow statement", elapsed: time.Second, want: slog.LevelWarn, msg: "sql slow"},
		{name: "failed statement", err: errors.New("syntax error"), want: slog.LevelError, msg: "sql failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{level: slog.LevelDebug}
			log := newLogger(slog.New(rec), Config{SlowQueryThreshold: 100 * time.Millisecond})

			log.Trace(context.Background(), time.Now().Add(-tc.elapsed),
				statement("SELECT * FROM widgets", nil), tc.err)

			got := rec.last(t)
			if got.Level != tc.want {
				t.Errorf("level = %v, want %v", got.Level, tc.want)
			}
			if got.Message != tc.msg {
				t.Errorf("message = %q, want %q", got.Message, tc.msg)
			}
			if !hasAttr(t, got, "statement") {
				t.Error("record has no statement attribute")
			}
			if !hasAttr(t, got, "duration") {
				t.Error("record has no duration attribute")
			}
			if ok := hasAttr(t, got, "error"); ok != (tc.err != nil) {
				t.Errorf("error attribute present = %v, want %v", ok, tc.err != nil)
			}
		})
	}
}

// A slow statement is a warning at any log level, which is the point of the
// threshold: it is the one statement worth seeing without turning on debug.
func TestSlowStatementIsLoggedAtInfoLevel(t *testing.T) {
	rec := &recorder{level: slog.LevelInfo}
	log := newLogger(slog.New(rec), Config{SlowQueryThreshold: 10 * time.Millisecond})

	log.Trace(context.Background(), time.Now().Add(-time.Second),
		statement("SELECT * FROM widgets", nil), nil)

	if got := rec.last(t); got.Message != "sql slow" {
		t.Errorf("message = %q, want %q", got.Message, "sql slow")
	}
}

// TestStatementTextIsNotBuiltWhenItWouldNotBeLogged is what makes per-statement
// logging free at a production level: GORM defers the SQL and the row count to a
// closure, and calling it anyway would pay for every line that is then dropped.
func TestStatementTextIsNotBuiltWhenItWouldNotBeLogged(t *testing.T) {
	rec := &recorder{level: slog.LevelInfo}
	log := newLogger(slog.New(rec), Config{SlowQueryThreshold: time.Hour})

	var built bool
	log.Trace(context.Background(), time.Now(), statement("SELECT 1", &built), nil)

	if built {
		t.Error("the statement was built for a record that was never emitted")
	}
	if len(rec.records) != 0 {
		t.Errorf("records = %d, want 0 at info level", len(rec.records))
	}
}

// db.Debug() asks for one statement to be logged, and it has to arrive at a level
// that is actually enabled — otherwise debugging a single query means raising the
// log level of the whole process.
func TestDebugModeLogsAtInfo(t *testing.T) {
	rec := &recorder{level: slog.LevelInfo}
	log := newLogger(slog.New(rec), Config{}).LogMode(gormlogger.Info)

	log.Trace(context.Background(), time.Now(), statement("SELECT 1", nil), nil)

	if got := rec.last(t); got.Level != slog.LevelInfo {
		t.Errorf("level = %v, want %v", got.Level, slog.LevelInfo)
	}
}

// A statement marked for debugging has to be visible even when its outcome is one
// of the quiet ones: "the row is not there" is frequently the thing being
// debugged, and leaving it at debug level would mean db.Debug() printed nothing.
func TestDebugModeShowsAQuietOutcome(t *testing.T) {
	rec := &recorder{level: slog.LevelInfo}
	log := newLogger(slog.New(rec), Config{}).LogMode(gormlogger.Info)

	log.Trace(context.Background(), time.Now(), statement("SELECT 1", nil), gorm.ErrRecordNotFound)

	got := rec.last(t)
	if got.Level != slog.LevelInfo {
		t.Errorf("level = %v, want %v", got.Level, slog.LevelInfo)
	}
	if got.Message != "sql not found" {
		t.Errorf("message = %q, want the outcome still classified as not found", got.Message)
	}
}

func TestSilentModeLogsNothing(t *testing.T) {
	rec := &recorder{level: slog.LevelDebug}
	log := newLogger(slog.New(rec), Config{}).LogMode(gormlogger.Silent)

	log.Trace(context.Background(), time.Now(), statement("SELECT 1", nil), errors.New("boom"))
	log.Error(context.Background(), "boom")

	if len(rec.records) != 0 {
		t.Errorf("records = %d, want 0 in silent mode", len(rec.records))
	}
}

// TestGormsOwnMessagesAreRendered covers the shape GORM's non-statement logging
// takes: a printf format string and its arguments, with a trailing newline meant
// for a line-oriented writer.
func TestGormsOwnMessagesAreRendered(t *testing.T) {
	rec := &recorder{level: slog.LevelDebug}
	log := newLogger(slog.New(rec), Config{})

	log.Warn(context.Background(), "removing callback `%s` from %s\n", "gorm:query", "repo.go:12")

	got := rec.last(t)
	if got.Message != "removing callback `gorm:query` from repo.go:12" {
		t.Errorf("message = %q, want the arguments substituted and the newline dropped", got.Message)
	}
	if hasAttr(t, got, "data") {
		t.Error("record carries a data attribute, want the arguments in the message instead")
	}
}

// TestParamsFilterKeepsValuesOutOfLogs covers the default that decides whether
// the rows themselves end up in a log aggregator.
func TestParamsFilterKeepsValuesOutOfLogs(t *testing.T) {
	const sql = "SELECT * FROM users WHERE email = $1"

	filter, ok := newLogger(slog.New(&recorder{}), Config{}).(gorm.ParamsFilter)
	if !ok {
		t.Fatal("the logger does not implement gorm.ParamsFilter, so GORM will log bound values")
	}
	if _, params := filter.ParamsFilter(context.Background(), sql, "a@example.com"); params != nil {
		t.Errorf("params = %v, want none: values must not reach a log line by default", params)
	}

	filter = newLogger(slog.New(&recorder{}), Config{IncludeQueryValues: true}).(gorm.ParamsFilter)
	if _, params := filter.ParamsFilter(context.Background(), sql, "a@example.com"); len(params) != 1 {
		t.Errorf("params = %v, want the values when IncludeQueryValues is set", params)
	}
}

// TestCallerPointsAtTheCallingCode is why the records are built by hand. slog
// resolves the call site from Record.PC, and GORM calls the logger from its own
// callback chain, so without this every statement in the service reports the same
// frame inside this package.
func TestCallerPointsAtTheCallingCode(t *testing.T) {
	rec := &recorder{level: slog.LevelDebug}
	conn, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("new sqlmock: %v", err)
	}
	mock.ExpectPing()
	mock.ExpectQuery("SELECT 1").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))

	db, err := Open(context.Background(), Config{}, WithLogger(slog.New(rec)), withMockDialector(conn))
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

	var pc uintptr
	for _, r := range rec.records {
		if r.Message == "sql" {
			pc = r.PC
		}
	}
	if pc == 0 {
		t.Fatal("the statement record carries no call site")
	}
	frame, _ := runtime.CallersFrames([]uintptr{pc}).Next()
	if !strings.HasSuffix(frame.File, "logger_test.go") {
		t.Errorf("caller = %s:%d, want the code that ran the query", frame.File, frame.Line)
	}
}
