package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/koungkub/tehran/internal/campaign"
)

const eventID = "33333333-3333-4333-8333-333333333333"

func testEvent() *campaign.Event {
	return &campaign.Event{
		ID: eventID, CampaignID: campaignID, Name: "signup", Type: "conversion",
		Payload: json.RawMessage(`{"amount":10}`),
		// A backfill: occurred_at is in the past while created_at is the write time,
		// which is the pair the list order depends on being distinct.
		OccurredAt: testTime.Add(-72 * time.Hour),
		CreatedAt:  testTime,
		UpdatedAt:  testTime,
	}
}

// eventRows builds a result set in the column order the entity declares.
func eventRows(ids ...string) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{
		"id", "campaign_id", "name", "type", "payload", "occurred_at", "created_at", "updated_at",
	})
	for i, id := range ids {
		// occurred_at descends per row while created_at is the same on all of them,
		// which is what lets a test tell the two columns apart in a cursor.
		rows.AddRow(id, campaignID, "signup", "conversion", []byte(`{"amount":10}`),
			testTime.Add(-time.Duration(i+1)*time.Minute), testTime, testTime)
	}
	return rows
}

// TestEventCreateForeignKey is the database's half of "every event must have a
// campaign": the service checks the parent first, and this is what happens when the
// campaign is deleted between that check and the insert.
func TestEventCreateForeignKey(t *testing.T) {
	t.Run("a foreign key violation becomes ErrCampaignNotFound", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectBegin()
		mock.ExpectExec(`INSERT INTO "events"`).
			WillReturnError(&pgconn.PgError{
				Code:    "23503",
				Message: `insert or update on table "events" violates foreign key constraint "events_campaign_id_fkey"`,
			})
		mock.ExpectRollback()

		err := NewEventRepository(db).Create(context.Background(), testEvent())
		if !errors.Is(err, campaign.ErrCampaignNotFound) {
			t.Fatalf("err = %v, want campaign.ErrCampaignNotFound", err)
		}
		// Reported as the parent being missing, not as a not-found event and not as
		// an opaque driver error.
		if errors.Is(err, campaign.ErrNotFound) {
			t.Error("a missing parent matched ErrNotFound")
		}
	})

	t.Run("any other driver error is not translated", func(t *testing.T) {
		db, mock := newMockDB(t)
		boom := &pgconn.PgError{Code: "23505", Message: "duplicate key"}
		mock.ExpectBegin()
		mock.ExpectExec(`INSERT INTO "events"`).WillReturnError(boom)
		mock.ExpectRollback()

		err := NewEventRepository(db).Create(context.Background(), testEvent())
		if errors.Is(err, campaign.ErrCampaignNotFound) || errors.Is(err, campaign.ErrNotFound) {
			t.Errorf("err = %v, want it left as the driver's own", err)
		}
		if !errors.Is(err, boom) {
			t.Errorf("err = %v, want it to wrap %v", err, boom)
		}
	})

	t.Run("a successful insert", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectBegin()
		mock.ExpectExec(`INSERT INTO "events"`).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		if err := NewEventRepository(db).Create(context.Background(), testEvent()); err != nil {
			t.Fatal(err)
		}
	})
}

// TestEventListByCampaign checks the predicate order the index depends on:
// campaign_id equality first, then the keyset range on (occurred_at, id).
func TestEventListByCampaign(t *testing.T) {
	t.Run("scoped to one campaign, ordered by occurred_at", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectQuery(
			`SELECT \* FROM "events" WHERE campaign_id = \$1 ORDER BY occurred_at DESC, id DESC LIMIT \$2`).
			WithArgs(campaignID, 3).
			WillReturnRows(eventRows(eventID))

		rows, next, err := NewEventRepository(db).ListByCampaign(context.Background(), campaignID, campaign.Page{Size: 2})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].CampaignID != campaignID {
			t.Errorf("rows = %+v, want one under %s", rows, campaignID)
		}
		if next != nil {
			t.Errorf("cursor = %+v, want nil", next)
		}
	})

	t.Run("the cursor predicate follows the equality", func(t *testing.T) {
		db, mock := newMockDB(t)
		cursorTime := testTime.Add(-time.Hour)
		mock.ExpectQuery(
			`SELECT \* FROM "events" WHERE campaign_id = \$1 AND \(occurred_at, id\) < \(\$2, \$3\) ORDER BY occurred_at DESC, id DESC LIMIT \$4`).
			WithArgs(campaignID, cursorTime, eventID, 2).
			WillReturnRows(eventRows())

		_, _, err := NewEventRepository(db).ListByCampaign(context.Background(), campaignID, campaign.Page{
			Size:   1,
			Cursor: &campaign.Cursor{Time: cursorTime, ID: eventID},
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("the cursor is built from occurred_at, not created_at", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectQuery(`SELECT \* FROM "events"`).
			WillReturnRows(eventRows(eventID, secondID))

		rows, next, err := NewEventRepository(db).ListByCampaign(context.Background(), campaignID, campaign.Page{Size: 1})
		if err != nil {
			t.Fatal(err)
		}
		if next == nil {
			t.Fatal("cursor = nil, want one")
		}
		// The fixture's created_at is the same on every row while occurred_at differs,
		// so a cursor taken from the wrong column would silently skip rows.
		if !next.Time.Equal(rows[0].OccurredAt) {
			t.Errorf("cursor time = %v, want the row's occurred_at %v", next.Time, rows[0].OccurredAt)
		}
		if next.Time.Equal(rows[0].CreatedAt) {
			t.Error("cursor was built from created_at")
		}
	})
}

// TestEventUpdateCannotReparent is the third place the immutable-parent rule is
// enforced: campaign_id is not in the column list, so no update can move an event
// however it is called.
func TestEventUpdateCannotReparent(t *testing.T) {
	for _, col := range eventMutableColumns {
		if col == "campaign_id" {
			t.Fatal("campaign_id is in the update column list; an event could be reparented")
		}
	}

	db, mock := newMockDB(t)
	mock.ExpectBegin()
	mock.ExpectExec(
		`UPDATE "events" SET "name"=\$1,"type"=\$2,"payload"=\$3,"occurred_at"=\$4,"updated_at"=\$5 WHERE id = \$6`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	e := testEvent()
	// Even with a different parent on the entity, the statement never carries it.
	e.CampaignID = secondID
	if err := NewEventRepository(db).Update(context.Background(), e); err != nil {
		t.Fatal(err)
	}
}

func TestEventUpdateAndDeleteRowsAffected(t *testing.T) {
	t.Run("an update matching nothing is a not-found", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectBegin()
		mock.ExpectExec(`UPDATE "events" SET`).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectCommit()

		err := NewEventRepository(db).Update(context.Background(), testEvent())
		if !errors.Is(err, campaign.ErrNotFound) {
			t.Fatalf("err = %v, want campaign.ErrNotFound", err)
		}
	})

	t.Run("a delete matching nothing is a not-found", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectBegin()
		mock.ExpectExec(`DELETE FROM "events" WHERE id = \$1`).
			WithArgs(eventID).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectCommit()

		err := NewEventRepository(db).Delete(context.Background(), eventID)
		if !errors.Is(err, campaign.ErrNotFound) {
			t.Fatalf("err = %v, want campaign.ErrNotFound", err)
		}
	})

	t.Run("a delete that removed a row succeeds", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectBegin()
		mock.ExpectExec(`DELETE FROM "events"`).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		if err := NewEventRepository(db).Delete(context.Background(), eventID); err != nil {
			t.Fatal(err)
		}
	})
}

func TestEventGetAndCount(t *testing.T) {
	t.Run("get returns the payload as stored", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectQuery(`SELECT \* FROM "events" WHERE id = \$1`).
			WithArgs(eventID, 1).
			WillReturnRows(eventRows(eventID))

		got, err := NewEventRepository(db).Get(context.Background(), eventID)
		if err != nil {
			t.Fatal(err)
		}
		if string(got.Payload) != `{"amount":10}` {
			t.Errorf("payload = %q, want the stored document", got.Payload)
		}
	})

	t.Run("get with no row is a not-found", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectQuery(`SELECT \* FROM "events"`).WillReturnRows(eventRows())

		_, err := NewEventRepository(db).Get(context.Background(), eventID)
		if !errors.Is(err, campaign.ErrNotFound) {
			t.Fatalf("err = %v, want campaign.ErrNotFound", err)
		}
	})

	t.Run("count is scoped to the campaign", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectQuery(`SELECT count\(\*\) FROM "events" WHERE campaign_id = \$1`).
			WithArgs(campaignID).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(7))

		got, err := NewEventRepository(db).CountByCampaign(context.Background(), campaignID)
		if err != nil {
			t.Fatal(err)
		}
		if got != 7 {
			t.Errorf("count = %d, want 7", got)
		}
	})
}
