package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/pressly/goose/v3"

	"github.com/koungkub/tehran/pkg/database"
)

// mockPool is a pool nothing is expected to be run against. goose.NewProvider
// collects migrations and builds a store without touching the database — no ping,
// no version table — so every test below that only needs a *sql.DB can use one.
func mockPool(t *testing.T) *sql.DB {
	t.Helper()
	conn, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("new sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// oneMigration is the smallest filesystem New accepts: goose globs *.sql at the
// root and refuses to build a provider from none.
func oneMigration() fstest.MapFS {
	return fstest.MapFS{
		"00001_init.sql": &fstest.MapFile{Data: []byte("-- +goose Up\nSELECT 1;\n")},
	}
}

func TestConfigDefaults(t *testing.T) {
	// The library constants, not literals: a literal would keep passing while
	// this test pinned a value the package had moved on from.
	cfg := Config{}.withDefaults()
	if cfg.Dialect != DefaultDialect {
		t.Errorf("dialect = %q, want %q", cfg.Dialect, DefaultDialect)
	}
	if cfg.TableName != DefaultTableName {
		t.Errorf("table_name = %q, want %q", cfg.TableName, DefaultTableName)
	}
	if cfg.LockMode != DefaultLockMode {
		t.Errorf("lock_mode = %q, want %q", cfg.LockMode, DefaultLockMode)
	}
	if cfg.LockID != DefaultLockID {
		t.Errorf("lock_id = %d, want %d", cfg.LockID, DefaultLockID)
	}
	if cfg.LockWait != DefaultLockWait {
		t.Errorf("lock_wait = %v, want %v", cfg.LockWait, DefaultLockWait)
	}

	// Timeout is the one duration that is not defaulted. Every other one here
	// treats a non-positive value as "use the default", so a zero surviving
	// withDefaults is the assertion, not an oversight.
	if cfg.Timeout != 0 {
		t.Errorf("timeout = %v, want 0 — a default here would cap a legitimate index build", cfg.Timeout)
	}
}

// TestLockWaitRoundsUpToARetryCount covers the conversion goose forces: it takes a
// retry period and a count rather than a duration, and a wait shorter than one
// period must still buy one attempt rather than none.
func TestLockWaitRoundsUpToARetryCount(t *testing.T) {
	tests := []struct {
		wait time.Duration
		want uint64
	}{
		{wait: 1 * time.Nanosecond, want: 1},
		{wait: 5 * time.Second, want: 1},
		{wait: 6 * time.Second, want: 2},
		{wait: 5 * time.Minute, want: 60},
	}
	for _, tc := range tests {
		t.Run(tc.wait.String(), func(t *testing.T) {
			got := Config{LockWait: tc.wait}.lockRetries()
			if got != tc.want {
				t.Errorf("lockRetries() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestLockModeAboveNoneRefusesANonPostgresDialect is the whole point of the
// dialect check: goose implements no locker for MySQL, so the alternative to
// refusing is starting up with locking silently absent — and two runners applying
// the same migration is not a failure anybody sees in a log line afterwards.
func TestLockModeAboveNoneRefusesANonPostgresDialect(t *testing.T) {
	for _, mode := range []string{LockModeSession, LockModeTable} {
		t.Run(mode, func(t *testing.T) {
			_, err := New(mockPool(t), oneMigration(), Config{Dialect: DialectMySQL, LockMode: mode})
			if err == nil {
				t.Fatalf("New with dialect %q and lock_mode %q succeeded, want an error",
					DialectMySQL, mode)
			}
			// The message has to name the way out, or the only actionable thing
			// about it is that it happened.
			if !strings.Contains(err.Error(), LockModeNone) {
				t.Errorf("error does not point at lock_mode %q: %v", LockModeNone, err)
			}
		})
	}

	// MySQL is usable — it just has to say so.
	if _, err := New(mockPool(t), oneMigration(), Config{
		Dialect:  DialectMySQL,
		LockMode: LockModeNone,
	}); err != nil {
		t.Errorf("New with dialect %q and lock_mode %q: %v", DialectMySQL, LockModeNone, err)
	}
}

func TestUnknownLockModeIsRejected(t *testing.T) {
	_, err := New(mockPool(t), oneMigration(), Config{LockMode: "advisory"})
	if err == nil {
		t.Fatal("New with lock_mode \"advisory\" succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "lock_mode") {
		t.Errorf("error does not name the setting: %v", err)
	}
}

func TestNewRequiresAPoolAndAFilesystem(t *testing.T) {
	if _, err := New(nil, oneMigration(), Config{}); err == nil {
		t.Error("New with a nil pool succeeded, want an error")
	}
	if _, err := New(mockPool(t), nil, Config{}); err == nil {
		t.Error("New with a nil filesystem succeeded, want an error")
	}
}

// TestEmptyFilesystemNamesTheSubdirectoryTrap covers the failure this package
// exists to make legible. goose globs the root of the filesystem it is given and
// does not recurse, so an embed.FS declared with a directory prefix looks empty
// from inside goose — which reports "no migrations found" for a directory that
// visibly has some.
func TestEmptyFilesystemNamesTheSubdirectoryTrap(t *testing.T) {
	nested := fstest.MapFS{
		"migrations/00001_init.sql": &fstest.MapFile{Data: []byte("-- +goose Up\nSELECT 1;\n")},
	}
	_, err := New(mockPool(t), nested, Config{})
	if err == nil {
		t.Fatal("New over a filesystem with no migration in its root succeeded, want an error")
	}
	if !errors.Is(err, goose.ErrNoMigrations) {
		t.Errorf("error does not wrap goose.ErrNoMigrations: %v", err)
	}
	if !strings.Contains(err.Error(), "fs.Sub") {
		t.Errorf("error does not name the fix: %v", err)
	}
}

func TestNewRejectsAnUnknownDialect(t *testing.T) {
	if _, err := New(mockPool(t), oneMigration(), Config{
		Dialect:  "cassandra",
		LockMode: LockModeNone,
	}); err == nil {
		t.Error("New with an unknown dialect succeeded, want an error")
	}
}

// TestGoMigrationsJoinTheSameSequence covers the merge: Go and SQL migrations are
// one ordered sequence, not two, and a Go migration passed in has to be visible
// among the sources rather than applied after them.
func TestGoMigrationsJoinTheSameSequence(t *testing.T) {
	fsys := fstest.MapFS{
		"00001_init.sql": &fstest.MapFile{Data: []byte("-- +goose Up\nSELECT 1;\n")},
		"00003_more.sql": &fstest.MapFile{Data: []byte("-- +goose Up\nSELECT 3;\n")},
	}
	m, err := New(mockPool(t), fsys, Config{LockMode: LockModeNone},
		WithGoMigrations(goose.NewGoMigration(2, nil, nil)),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var got []int64
	for _, s := range m.Sources() {
		got = append(got, s.Version)
	}
	want := []int64{1, 2, 3}
	if len(got) != len(want) {
		t.Fatalf("versions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("versions = %v, want %v", got, want)
		}
	}
}

// TestPartialFailureReportsWhatWasApplied guards the one piece of goose's error
// handling that is easy to drop on the floor. goose returns a nil result slice
// alongside a PartialError, so a wrapper that returned `results, err` verbatim
// would report a half-finished run as having applied nothing at all — which is
// the opposite of true and the first thing anybody diagnosing one looks at.
func TestPartialFailureReportsWhatWasApplied(t *testing.T) {
	partial := &goose.PartialError{
		Applied: []*goose.MigrationResult{{
			Source:    &goose.Source{Version: 1, Path: "00001_init.sql"},
			Direction: "up",
			Duration:  time.Millisecond,
		}},
		Failed: &goose.MigrationResult{
			Source: &goose.Source{Version: 2, Path: "00002_boom.sql"},
		},
		Err: errors.New("syntax error at or near \"boom\""),
	}

	applied, err := appliedFrom(nil, partial)
	if err == nil {
		t.Fatal("appliedFrom returned no error for a PartialError")
	}
	// Wrapped, not replaced: a caller that wants the failing migration reaches
	// for it through errors.As.
	if _, ok := errors.AsType[*goose.PartialError](err); !ok {
		t.Errorf("error no longer unwraps to *goose.PartialError: %v", err)
	}
	if len(applied) != 1 {
		t.Fatalf("applied = %d migrations, want the 1 that landed before the failure", len(applied))
	}
	if applied[0].Version != 1 || applied[0].Source != "00001_init.sql" {
		t.Errorf("applied[0] = %+v, want version 1 from 00001_init.sql", applied[0])
	}
}

func TestAppliedFromPassesSuccessThrough(t *testing.T) {
	results := []*goose.MigrationResult{{
		Source:    &goose.Source{Version: 7, Path: "00007_widgets.sql"},
		Direction: "up",
		Duration:  2 * time.Millisecond,
		Empty:     true,
	}}
	applied, err := appliedFrom(results, nil)
	if err != nil {
		t.Fatalf("appliedFrom: %v", err)
	}
	if len(applied) != 1 {
		t.Fatalf("applied = %d, want 1", len(applied))
	}
	want := Applied{Version: 7, Source: "00007_widgets.sql", Direction: "up", Duration: 2 * time.Millisecond, Empty: true}
	if applied[0] != want {
		t.Errorf("applied[0] = %+v, want %+v", applied[0], want)
	}
}

// TestTimeoutIsOffUnlessConfigured covers both halves of Config.Timeout, since the
// off case is the default and a bound that appeared by accident would cut a long
// index build short with nothing in the configuration to explain it.
func TestTimeoutIsOffUnlessConfigured(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		m, err := New(mockPool(t), oneMigration(), Config{LockMode: LockModeNone})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		ctx, cancel := m.bound(context.Background())
		defer cancel()
		if deadline, ok := ctx.Deadline(); ok {
			t.Errorf("context has deadline %v, want none", deadline)
		}
	})

	t.Run("configured", func(t *testing.T) {
		m, err := New(mockPool(t), oneMigration(), Config{LockMode: LockModeNone, Timeout: time.Minute})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		ctx, cancel := m.bound(context.Background())
		defer cancel()
		if _, ok := ctx.Deadline(); !ok {
			t.Error("context has no deadline, want one from timeout")
		}
	})
}

// testDSN gates the one test here that needs a real database.
//
// Everything above runs against a mock pool, because goose builds a provider
// without touching the database — which covers the wiring and none of the SQL.
// What is only reachable with a server on the other end is the part that matters
// most: that the version table is created and read back, that an out-of-order
// version is refused, and that a failing migration leaves its predecessors
// applied. Point this at a throwaway database to run it:
//
//	TEHRAN_TEST_DATABASE_DSN='postgres://tehran:tehran@127.0.0.1:5432/tehran?sslmode=disable' \
//	    go test ./pkg/migrate/ -run EndToEnd -v
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEHRAN_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("TEHRAN_TEST_DATABASE_DSN is not set; skipping the end-to-end migration test")
	}
	return dsn
}

// endToEndPool opens a real pool and gives every run its own version table, so a
// repeat run is not fighting the rows the last one left behind.
func endToEndPool(t *testing.T) (*sql.DB, string) {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, database.Config{DSN: testDSN(t)}, database.WithName("migrate-test"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	table := "goose_test_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	t.Cleanup(func() {
		// Best effort: a failed test is more useful with its rows still there,
		// and a leftover table in a throwaway database costs nothing.
		_, _ = db.SQL().ExecContext(context.Background(), "DROP TABLE IF EXISTS "+table)
	})
	return db.SQL(), table
}

func TestEndToEndAppliesAndReportsMigrations(t *testing.T) {
	pool, table := endToEndPool(t)
	widgets := "widgets_" + strings.TrimPrefix(table, "goose_test_")
	t.Cleanup(func() {
		_, _ = pool.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+widgets)
	})

	fsys := fstest.MapFS{
		"00001_empty.sql": &fstest.MapFile{Data: []byte("-- +goose Up\n\n-- +goose Down\n")},
		"00002_widgets.sql": &fstest.MapFile{Data: fmt.Appendf(nil,
			"-- +goose Up\nCREATE TABLE %s (id bigint primary key);\n\n-- +goose Down\nDROP TABLE %s;\n",
			widgets, widgets)},
	}
	m, err := New(pool, fsys, Config{TableName: table})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	applied, err := m.Up(ctx)
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if len(applied) != 2 {
		t.Fatalf("Up applied %d migrations, want 2", len(applied))
	}
	// The empty baseline is recorded, and reports itself as empty — the case the
	// repository's own first migration relies on.
	if !applied[0].Empty {
		t.Errorf("applied[0] = %+v, want Empty for a migration with no statements", applied[0])
	}
	if applied[1].Empty {
		t.Errorf("applied[1] = %+v, want Empty false for a CREATE TABLE", applied[1])
	}

	version, err := m.Version(ctx)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if version != 2 {
		t.Errorf("Version = %d, want 2", version)
	}

	pending, err := m.HasPending(ctx)
	if err != nil {
		t.Fatalf("HasPending: %v", err)
	}
	if pending {
		t.Error("HasPending = true after a complete Up")
	}

	statuses, err := m.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("Status = %d rows, want 2", len(statuses))
	}
	for _, s := range statuses {
		if !s.Applied {
			t.Errorf("status %+v reports not applied after Up", s)
		}
		if s.AppliedAt.IsZero() {
			t.Errorf("status %+v has a zero AppliedAt", s)
		}
	}

	// A second Up is a no-op rather than an error: a migration job that is retried
	// after a network blip has to be safe to re-run.
	again, err := m.Up(ctx)
	if err != nil {
		t.Fatalf("second Up: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second Up applied %d migrations, want 0", len(again))
	}

	down, err := m.Down(ctx)
	if err != nil {
		t.Fatalf("Down: %v", err)
	}
	if down.Version != 2 {
		t.Errorf("Down rolled back version %d, want the highest applied, 2", down.Version)
	}
}

// TestEndToEndPartialFailureLeavesPredecessorsApplied is the behaviour the
// PartialError handling exists for, against a database that really does reject
// the second statement.
func TestEndToEndPartialFailureLeavesPredecessorsApplied(t *testing.T) {
	pool, table := endToEndPool(t)
	fsys := fstest.MapFS{
		"00001_ok.sql":   &fstest.MapFile{Data: []byte("-- +goose Up\nSELECT 1;\n")},
		"00002_boom.sql": &fstest.MapFile{Data: []byte("-- +goose Up\nNOT VALID SQL;\n")},
	}
	m, err := New(pool, fsys, Config{TableName: table})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	applied, err := m.Up(ctx)
	if err == nil {
		t.Fatal("Up over an invalid migration succeeded")
	}
	if len(applied) != 1 || applied[0].Version != 1 {
		t.Fatalf("applied = %+v, want just version 1", applied)
	}

	// And the version table agrees: the failure rolled back its own migration
	// without touching the one before it.
	version, err := m.Version(ctx)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if version != 1 {
		t.Errorf("Version = %d, want 1", version)
	}
}

// TestEndToEndSessionLockSerialisesConcurrentRunners is the reason LockMode
// defaults to something rather than nothing: two replicas of a migration Job
// starting together is the ordinary case, not the pathological one.
//
// Unlocked, both runners see the same migration pending and both try to apply it,
// and the loser fails on the version table's primary key — after having run the
// statements. Locked, the second waits, finds nothing left to do, and exits zero,
// which is what makes a Job with more than one attempt safe. It is slow by
// construction: goose retries the lock on a fixed five-second period, so the
// runner that loses spends one of those waiting.
func TestEndToEndSessionLockSerialisesConcurrentRunners(t *testing.T) {
	pool, table := endToEndPool(t)
	fsys := fstest.MapFS{
		// Long enough that the second runner cannot possibly slip past before the
		// first has taken the lock.
		"00001_slow.sql": &fstest.MapFile{Data: []byte("-- +goose Up\nSELECT pg_sleep(1);\n")},
	}

	// Two Migrators, as two processes would be — not one shared between
	// goroutines, which would prove nothing about the lock.
	runners := make([]*Migrator, 2)
	for i := range runners {
		m, err := New(pool, fsys, Config{TableName: table, LockMode: LockModeSession})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		runners[i] = m
	}

	type outcome struct {
		applied int
		err     error
	}
	results := make(chan outcome, len(runners))
	for _, m := range runners {
		go func() {
			applied, err := m.Up(context.Background())
			results <- outcome{applied: len(applied), err: err}
		}()
	}

	total := 0
	for range runners {
		got := <-results
		if got.err != nil {
			t.Errorf("Up: %v", got.err)
		}
		total += got.applied
	}
	if total != 1 {
		t.Errorf("the two runners applied %d migrations between them, want exactly 1", total)
	}
}

// TestEndToEndReadsDoNotQueueBehindARunningMigration is a regression test for a
// defect that only a running database shows.
//
// goose enables locking per provider and then takes the lock inside Status and
// GetDBVersion, so a Migrator with one provider makes `db status` wait out the
// whole of lock_wait — five minutes by default — while a migration is in progress.
// That is precisely when somebody runs it. The second, lock-free provider is what
// this asserts: reads answer immediately while a migration holds the lock.
func TestEndToEndReadsDoNotQueueBehindARunningMigration(t *testing.T) {
	pool, table := endToEndPool(t)
	fsys := fstest.MapFS{
		"00001_slow.sql": &fstest.MapFile{Data: []byte("-- +goose Up\nSELECT pg_sleep(2);\n")},
	}
	writer, err := New(pool, fsys, Config{TableName: table, LockMode: LockModeSession})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reader, err := New(pool, fsys, Config{TableName: table, LockMode: LockModeSession})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, upErr := writer.Up(context.Background())
		done <- upErr
	}()
	// Long enough for the migration to have taken the lock, short enough to leave
	// most of its two seconds still to run.
	<-time.After(500 * time.Millisecond)

	// The bar is deliberately far below goose's five-second lock retry period and
	// far above what these three reads cost: anything in between means a read took
	// the lock.
	const bar = 2 * time.Second
	ctx := context.Background()
	for _, read := range []struct {
		name string
		call func() error
	}{
		{"Status", func() error { _, err := reader.Status(ctx); return err }},
		{"Version", func() error { _, err := reader.Version(ctx); return err }},
		{"HasPending", func() error { _, err := reader.HasPending(ctx); return err }},
	} {
		start := time.Now()
		if err := read.call(); err != nil {
			t.Errorf("%s: %v", read.name, err)
		}
		if elapsed := time.Since(start); elapsed > bar {
			t.Errorf("%s took %v while a migration held the lock, want under %v: it is waiting for the lock",
				read.name, elapsed, bar)
		}
	}

	if err := <-done; err != nil {
		t.Errorf("Up: %v", err)
	}
}

// TestEndToEndRefusesAnOutOfOrderMigration covers AllowOutOfOrder being off: a
// version below the highest applied is the merge-collision case, and applying it
// quietly leaves two environments on the same version number with a different
// schema.
func TestEndToEndRefusesAnOutOfOrderMigration(t *testing.T) {
	pool, table := endToEndPool(t)
	ctx := context.Background()

	first, err := New(pool, fstest.MapFS{
		"00001_a.sql": &fstest.MapFile{Data: []byte("-- +goose Up\nSELECT 1;\n")},
		"00003_c.sql": &fstest.MapFile{Data: []byte("-- +goose Up\nSELECT 3;\n")},
	}, Config{TableName: table})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := first.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}

	// Version 2 arrives afterwards, as it does when two branches merge.
	interleaved := fstest.MapFS{
		"00001_a.sql": &fstest.MapFile{Data: []byte("-- +goose Up\nSELECT 1;\n")},
		"00002_b.sql": &fstest.MapFile{Data: []byte("-- +goose Up\nSELECT 2;\n")},
		"00003_c.sql": &fstest.MapFile{Data: []byte("-- +goose Up\nSELECT 3;\n")},
	}

	refusing, err := New(pool, interleaved, Config{TableName: table})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := refusing.Up(ctx); err == nil {
		t.Error("Up applied an out-of-order migration with allow_out_of_order off")
	}

	allowing, err := New(pool, interleaved, Config{TableName: table, AllowOutOfOrder: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	applied, err := allowing.Up(ctx)
	if err != nil {
		t.Fatalf("Up with allow_out_of_order: %v", err)
	}
	if len(applied) != 1 || applied[0].Version != 2 {
		t.Errorf("applied = %+v, want just the missing version 2", applied)
	}
}
