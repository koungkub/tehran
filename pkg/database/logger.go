package database

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// gormLogger adapts GORM's logger interface onto *slog.Logger, so a service gets
// its statements in the same stream, format and correlation as everything else
// it logs, and this package exposes no logging type of its own.
type gormLogger struct {
	log           *slog.Logger
	slowThreshold time.Duration
	includeValues bool
	// verbose logs every statement at info rather than debug. It is set by
	// LogMode(Info), which is what db.Debug() does — so a single query marked
	// for debugging is visible at a production log level, without turning on
	// debug for the process.
	verbose bool
	silent  bool
}

func newLogger(log *slog.Logger, cfg Config) gormlogger.Interface {
	return gormLogger{
		log:           log,
		slowThreshold: cfg.SlowQueryThreshold,
		includeValues: cfg.IncludeQueryValues,
	}
}

// LogMode is how GORM asks for a more or less verbose logger — db.Debug() and a
// Session with an explicit LogLevel both come through here.
func (l gormLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	l.verbose = level >= gormlogger.Info
	l.silent = level <= gormlogger.Silent
	return l
}

// Info, Warn and Error carry GORM's own messages — a callback replaced, a
// dialector that cannot translate errors — not statements.

func (l gormLogger) Info(ctx context.Context, msg string, args ...any) {
	l.emit(ctx, slog.LevelInfo, message(msg, args))
}

func (l gormLogger) Warn(ctx context.Context, msg string, args ...any) {
	l.emit(ctx, slog.LevelWarn, message(msg, args))
}

func (l gormLogger) Error(ctx context.Context, msg string, args ...any) {
	l.emit(ctx, slog.LevelError, message(msg, args))
}

// message renders what GORM passed, which is a printf format string and its
// arguments — "removing callback `%s` from %s\n" and the two values. Carrying the
// arguments as a separate attribute instead would leave the verbs unsubstituted
// in the message, so the line would read `%s` where the callback's name belongs.
// The trailing newline GORM includes is dropped: it is meant for a line-oriented
// writer, not for a structured record's message field.
func message(msg string, args []any) string {
	if len(args) > 0 {
		msg = fmt.Sprintf(msg, args...)
	}
	return strings.TrimSpace(msg)
}

// Trace is called once per statement, after it has run.
//
// The level a statement lands at is a classification, not a formality, the same
// way an RPC's code is: a query that found nothing and a query cut short because
// the caller hung up are outcomes, not faults, and logging them at error level
// trains an on-call rotation to ignore database errors. Only a statement that
// genuinely failed is an error here.
func (l gormLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	if l.silent {
		return
	}
	elapsed := time.Since(begin)
	level, msg := slog.LevelDebug, "sql"
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		// The documented result of First on an empty set. A repository decides
		// whether that is an error; the statement was not.
		msg = "sql not found"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The caller went away or ran out of time. Nothing is wrong with the
		// database, but a rise in these is worth seeing.
		level, msg = slog.LevelWarn, "sql aborted"
	case err != nil:
		level, msg = slog.LevelError, "sql failed"
	case l.slowThreshold > 0 && elapsed > l.slowThreshold:
		level, msg = slog.LevelWarn, "sql slow"
	}
	// db.Debug() asked for this statement specifically, so raise it to a level a
	// production logger has enabled — including the not-found case, where "the row
	// is not there" is often the very thing being debugged.
	if l.verbose && level < slog.LevelInfo {
		level = slog.LevelInfo
	}

	// fc builds the statement text and counts the rows, which is not free, so
	// check the level is actually enabled before calling it. This is the whole
	// reason statements cost nothing at the default level.
	if !l.log.Enabled(ctx, level) {
		return
	}
	statement, rows := fc()
	attrs := []slog.Attr{
		slog.String("statement", statement),
		// The elapsed time measured above, not a second reading: the level was
		// decided from the first one, and a line whose duration disagrees with
		// the threshold that classified it is worse than useless.
		slog.Duration("duration", elapsed),
	}
	// GORM reports -1 for a statement whose row count is not meaningful.
	if rows != -1 {
		attrs = append(attrs, slog.Int64("rows", rows))
	}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	l.emit(ctx, level, msg, attrs...)
}

// ParamsFilter is GORM's hook for deciding what a statement's bound values look
// like by the time they reach a log line. Returning no values leaves the
// placeholders in place, which keeps the data itself out of the logs.
func (l gormLogger) ParamsFilter(_ context.Context, sql string, params ...any) (string, []any) {
	if l.includeValues {
		return sql, params
	}
	return sql, nil
}

// emit builds the record by hand for one reason: Record.PC. A record created
// through slog.Logger's own methods would carry a call site inside this file, so
// every statement in the service would report the same one.
func (l gormLogger) emit(ctx context.Context, level slog.Level, msg string, attrs ...slog.Attr) {
	if l.silent || !l.log.Enabled(ctx, level) {
		return
	}
	r := slog.NewRecord(time.Now(), level, msg, callerPC())
	r.AddAttrs(attrs...)
	// Handle rather than one of slog.Logger's methods, which would build a
	// second record and discard the call site resolved above. Attributes added
	// with logger.With are held by the handler, so they survive this.
	_ = l.log.Handler().Handle(ctx, r)
}

// thisDir is this file's directory at compile time, used to recognise this
// package's own frames.
var thisDir = func() string {
	_, file, _, _ := runtime.Caller(0)
	return path.Dir(filepath.ToSlash(file)) + "/"
}()

// callerPC resolves the program counter of the code that ran the statement,
// which is the frame below both this package and GORM itself — a repository
// method, typically. GORM's own utils.CallerFrame stops at the first frame
// outside GORM, which from here would be this file.
//
// It returns 0 when no such frame is found, which slog treats as "no source
// information" rather than as an error.
func callerPC() uintptr {
	var pcs [16]uintptr
	// Skip runtime.Callers, callerPC and emit.
	n := runtime.Callers(4, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		if frame.PC == 0 {
			return 0
		}
		file := filepath.ToSlash(frame.File)
		// A test in this package is a caller like any other, so it is exempt
		// from the rule that skips this package's own frames — the same
		// exception GORM makes in its own caller resolution, and the only way
		// this behaviour can be asserted from here at all.
		switch {
		case strings.HasSuffix(file, "_test.go"):
			return frame.PC
		case !strings.HasPrefix(file, thisDir) && !strings.Contains(file, "/gorm.io/"):
			return frame.PC
		}
		if !more {
			return 0
		}
	}
}
