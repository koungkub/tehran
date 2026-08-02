// Package postgres is the persistence adapter: it implements the repository
// interfaces the domain packages declare, over the GORM handle that pkg/database
// owns.
//
// The dependency runs one way. A domain package says what it needs and names no
// database; this package imports the domain to satisfy it. Nothing imports this
// package except the composition root, which is the only place that decides which
// store a domain gets — .golangci.yml has a depguard rule for the direction, so
// getting it backwards is a lint failure rather than a review comment.
//
// Every query goes through WithContext(ctx). That is not optional here: the
// instrumentation pkg/database registers reads the context for the active span, so
// a statement issued without one produces a trace-less span and a log line with
// nothing to correlate it to.
package postgres

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// pgErrForeignKeyViolation is SQLSTATE 23503. It is what the events table's
// foreign key raises when an insert names a campaign that is not there — the race
// the service's own existence check cannot close, since the campaign can be
// deleted between the two statements.
const pgErrForeignKeyViolation = "23503"

// isForeignKeyViolation reports whether err is the constraint above.
//
// Matched on the driver's error type and its code, not on the message: the text is
// localised and carries the constraint name, so matching it would break on a
// server locale or a renamed constraint.
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgErrForeignKeyViolation
}
