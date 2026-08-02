package postgres

import (
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// These tests run the repositories against a mocked driver rather than a database.
// What that buys is the part unit tests usually cannot reach: the SQL these methods
// actually emit — the keyset predicate, the column list an update writes, the
// statements a delete puts in one transaction — asserted as text, plus the
// translation of driver outcomes into the domain's errors.
//
// What it does not buy is any assurance that the SQL is valid or that the index is
// used. That is what `make run` against compose.yaml is for, and the two are
// complementary rather than alternatives.

// newMockDB builds a GORM handle over sqlmock. The default matcher treats an
// expectation as a regular expression matched against the statement, so a test
// pins the parts that carry meaning — the predicate, the order, the column list —
// without also pinning GORM's quoting and placeholder numbering.
//
// Note that GORM wraps every write in its own transaction unless told not to, so a
// single Create or Update expects Begin and Commit around it.
func newMockDB(t *testing.T) (*gorm.DB, sqlmock.Sqlmock) {
	t.Helper()
	conn, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("new sqlmock: %v", err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet expectations: %v", err)
		}
		_ = conn.Close()
	})

	db, err := gorm.Open(postgres.New(postgres.Config{
		Conn: conn,
		// The mock answers no version query, and without this GORM asks for one to
		// decide whether it can use RETURNING.
		WithoutReturning: true,
	}), &gorm.Config{
		Logger: logger.Discard,
		// A prepared statement per query would put a Prepare between every
		// expectation and the statement it belongs to.
		PrepareStmt: false,
	})
	if err != nil {
		t.Fatalf("gorm open: %v", err)
	}
	return db, mock
}
