package database

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// gormLogger adapts GORM's logger interface onto *zerolog.Logger, so a service
// gets its statements in the same stream, format and correlation as everything
// else it logs, and this package exposes no logging type of its own.
type gormLogger struct {
	log           *zerolog.Logger
	slowThreshold time.Duration
	includeValues bool
	// verbose logs every statement at info rather than debug. It is set by
	// LogMode(Info), which is what db.Debug() does — so a single query marked
	// for debugging is visible at a production log level, without turning on
	// debug for the process.
	verbose bool
	silent  bool
}

func newLogger(log *zerolog.Logger, cfg Config) gormlogger.Interface {
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
	l.emit(ctx, zerolog.InfoLevel, message(msg, args))
}

func (l gormLogger) Warn(ctx context.Context, msg string, args ...any) {
	l.emit(ctx, zerolog.WarnLevel, message(msg, args))
}

func (l gormLogger) Error(ctx context.Context, msg string, args ...any) {
	l.emit(ctx, zerolog.ErrorLevel, message(msg, args))
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
	level, msg := zerolog.DebugLevel, "sql"
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		// The documented result of First on an empty set. A repository decides
		// whether that is an error; the statement was not.
		msg = "sql not found"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The caller went away or ran out of time. Nothing is wrong with the
		// database, but a rise in these is worth seeing.
		level, msg = zerolog.WarnLevel, "sql aborted"
	case err != nil:
		level, msg = zerolog.ErrorLevel, "sql failed"
	case l.slowThreshold > 0 && elapsed > l.slowThreshold:
		level, msg = zerolog.WarnLevel, "sql slow"
	}
	// db.Debug() asked for this statement specifically, so raise it to a level a
	// production logger has enabled — including the not-found case, where "the row
	// is not there" is often the very thing being debugged.
	if l.verbose && level < zerolog.InfoLevel {
		level = zerolog.InfoLevel
	}

	// fc builds the statement text and counts the rows, which is not free, so
	// check the level is actually enabled before calling it. WithLevel returns a
	// disabled event rather than nil when the level is off, and Enabled reports
	// which — this is the whole reason statements cost nothing at the default
	// level.
	e := l.log.WithLevel(level)
	if !e.Enabled() {
		return
	}
	statement, rows := fc()
	e.Ctx(ctx).
		Str("statement", statement).
		// The elapsed time measured above, not a second reading: the level was
		// decided from the first one, and a line whose duration disagrees with
		// the threshold that classified it is worse than useless.
		Dur("duration", elapsed)
	// GORM reports -1 for a statement whose row count is not meaningful.
	if rows != -1 {
		e.Int64("rows", rows)
	}
	if err != nil {
		e.Str("error", err.Error())
	}
	e.Msg(msg)
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

// emit is the plain path for GORM's own messages, which carry no fields.
func (l gormLogger) emit(ctx context.Context, level zerolog.Level, msg string) {
	if l.silent {
		return
	}
	e := l.log.WithLevel(level)
	if !e.Enabled() {
		return
	}
	e.Ctx(ctx).Msg(msg)
}
