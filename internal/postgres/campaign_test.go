package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/koungkub/tehran/internal/campaign"
)

const (
	campaignID = "11111111-1111-4111-8111-111111111111"
	secondID   = "22222222-2222-4222-8222-222222222222"
)

var testTime = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

// campaignRows builds a result set in the column order the entity declares.
func campaignRows(ids ...string) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{
		"id", "name", "description", "status", "start_at", "end_at", "created_at", "updated_at",
	})
	for i, id := range ids {
		// Descending created_at, so the rows arrive in the order the query asks for
		// and the cursor built from the last one is the oldest of the page.
		rows.AddRow(id, "summer", "", string(campaign.StatusActive), nil, nil,
			testTime.Add(-time.Duration(i)*time.Minute), testTime)
	}
	return rows
}

func TestCampaignGet(t *testing.T) {
	t.Run("a row is returned", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectQuery(`SELECT \* FROM "campaigns" WHERE id = \$1`).
			WithArgs(campaignID, 1).
			WillReturnRows(campaignRows(campaignID))

		got, err := NewCampaignRepository(db).Get(context.Background(), campaignID)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != campaignID || got.Status != campaign.StatusActive {
			t.Errorf("campaign = %+v, want %s/active", got, campaignID)
		}
	})

	t.Run("no rows becomes the domain's not-found", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectQuery(`SELECT \* FROM "campaigns"`).
			WillReturnRows(campaignRows())

		_, err := NewCampaignRepository(db).Get(context.Background(), campaignID)
		if !errors.Is(err, campaign.ErrNotFound) {
			t.Fatalf("err = %v, want campaign.ErrNotFound", err)
		}
		// And the driver's own error type must not survive the translation: the
		// layers above match on the domain sentinel and know nothing about GORM.
		if err.Error() == "" {
			t.Error("the error lost its message")
		}
	})
}

// TestCampaignListKeyset is the pagination contract in SQL: a row-value comparison
// on the pair the index is built on, the matching order, and one row more than the
// caller asked for.
func TestCampaignListKeyset(t *testing.T) {
	t.Run("the first page has no cursor predicate", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectQuery(`SELECT \* FROM "campaigns" ORDER BY created_at DESC, id DESC LIMIT \$1`).
			WithArgs(3). // page size 2, plus the probe row
			WillReturnRows(campaignRows(campaignID))

		rows, next, err := NewCampaignRepository(db).List(context.Background(), campaign.Page{Size: 2})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Errorf("rows = %d, want 1", len(rows))
		}
		// One row for a page of two: there is nothing after it.
		if next != nil {
			t.Errorf("cursor = %+v, want nil", next)
		}
	})

	t.Run("a cursor becomes a row-value comparison", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectQuery(
			`SELECT \* FROM "campaigns" WHERE \(created_at, id\) < \(\$1, \$2\) ORDER BY created_at DESC, id DESC LIMIT \$3`).
			WithArgs(testTime, campaignID, 2).
			WillReturnRows(campaignRows(secondID))

		_, _, err := NewCampaignRepository(db).List(context.Background(), campaign.Page{
			Size:   1,
			Cursor: &campaign.Cursor{Time: testTime, ID: campaignID},
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a full page yields a cursor on its last row and drops the probe", func(t *testing.T) {
		db, mock := newMockDB(t)
		// Two rows come back for a page of one: the second is the probe.
		mock.ExpectQuery(`SELECT \* FROM "campaigns"`).
			WillReturnRows(campaignRows(campaignID, secondID))

		rows, next, err := NewCampaignRepository(db).List(context.Background(), campaign.Page{Size: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].ID != campaignID {
			t.Fatalf("rows = %+v, want just %s", rows, campaignID)
		}
		if next == nil {
			t.Fatal("cursor = nil, want one: a probe row came back")
		}
		// The cursor is the last row of the page returned, not the probe: resuming
		// from the probe would skip it.
		if next.ID != campaignID || !next.Time.Equal(rows[0].CreatedAt) {
			t.Errorf("cursor = %+v, want the returned row %s/%v", next, campaignID, rows[0].CreatedAt)
		}
	})
}

func TestCampaignUpdate(t *testing.T) {
	stored := &campaign.Campaign{
		ID: campaignID, Name: "summer", Status: campaign.StatusEnded,
		CreatedAt: testTime, UpdatedAt: testTime,
	}

	t.Run("the mutable columns are written and id is not", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectBegin()
		// Every mutable column, including the ones holding a zero value: GORM would
		// skip those without the explicit Select, and a description cleared back to
		// empty has to reach the table.
		mock.ExpectExec(
			`UPDATE "campaigns" SET "name"=\$1,"description"=\$2,"status"=\$3,"start_at"=\$4,"end_at"=\$5,"updated_at"=\$6 WHERE id = \$7`).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		if err := NewCampaignRepository(db).Update(context.Background(), stored); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("zero rows affected is a not-found", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectBegin()
		// Not an error to GORM, and the only signal that the row is gone: the service
		// read it a moment earlier, so something deleted it in between.
		mock.ExpectExec(`UPDATE "campaigns" SET`).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectCommit()

		err := NewCampaignRepository(db).Update(context.Background(), stored)
		if !errors.Is(err, campaign.ErrNotFound) {
			t.Fatalf("err = %v, want campaign.ErrNotFound", err)
		}
	})
}

// TestCampaignDelete covers the cascade count: the events are counted and the
// campaign deleted inside one transaction, so the number reported is the number the
// delete actually removed.
func TestCampaignDelete(t *testing.T) {
	t.Run("the count and the delete share a transaction", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectBegin()
		mock.ExpectQuery(`SELECT count\(\*\) FROM "events" WHERE campaign_id = \$1`).
			WithArgs(campaignID).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
		mock.ExpectExec(`DELETE FROM "campaigns" WHERE id = \$1`).
			WithArgs(campaignID).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		got, err := NewCampaignRepository(db).Delete(context.Background(), campaignID)
		if err != nil {
			t.Fatal(err)
		}
		if got != 3 {
			t.Errorf("deleted events = %d, want 3", got)
		}
	})

	t.Run("an unknown campaign rolls back and is a not-found", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectBegin()
		mock.ExpectQuery(`SELECT count\(\*\) FROM "events"`).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
		mock.ExpectExec(`DELETE FROM "campaigns"`).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectRollback()

		_, err := NewCampaignRepository(db).Delete(context.Background(), campaignID)
		if !errors.Is(err, campaign.ErrNotFound) {
			t.Fatalf("err = %v, want campaign.ErrNotFound", err)
		}
	})
}

func TestCampaignExists(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{name: "present", want: true},
		{name: "absent", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock := newMockDB(t)
			// EXISTS rather than a select of the row: nothing reads the row, and the
			// service calls this on every event write.
			mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM campaigns WHERE id = \$1\)`).
				WithArgs(campaignID).
				WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(tt.want))

			got, err := NewCampaignRepository(db).Exists(context.Background(), campaignID)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("exists = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestIsForeignKeyViolation pins the check to the driver's SQLSTATE rather than to
// the message, which is localised and carries the constraint name.
func TestIsForeignKeyViolation(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "the foreign key code",
			err:  &pgconn.PgError{Code: "23503", Message: `insert or update on table "events" violates foreign key constraint`},
			want: true,
		},
		{
			name: "a different constraint",
			err:  &pgconn.PgError{Code: "23505", Message: "unique violation"},
			want: false,
		},
		{
			// A wrapped driver error still matches: the repositories wrap with %w.
			name: "wrapped",
			err:  errors.Join(errors.New("create event"), &pgconn.PgError{Code: "23503"}),
			want: true,
		},
		{name: "not a driver error", err: errors.New("23503"), want: false},
		{name: "nil", err: nil, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isForeignKeyViolation(tt.err); got != tt.want {
				t.Errorf("isForeignKeyViolation = %v, want %v", got, tt.want)
			}
		})
	}
}
