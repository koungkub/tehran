package campaign

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// nopLogger keeps these tests off whatever zerolog's package-level logger happens
// to point at.
var nopLogger = zerolog.Nop()

// A valid uuid to address rows with. The service parses ids before using them, so
// a test that passes "c1" would be testing the parse and nothing else.
const (
	campaignID = "11111111-1111-4111-8111-111111111111"
	otherID    = "22222222-2222-4222-8222-222222222222"
	eventID    = "33333333-3333-4333-8333-333333333333"
)

// encodeRaw base64s a token body directly, so a test can hand DecodeCursor a
// well-formed encoding of a malformed payload — which Cursor.Encode cannot produce.
func encodeRaw(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

// fixedNow pins the clock for the duration of a test, so a stamped row can be
// compared against a known value.
func fixedNow(t *testing.T, at time.Time) {
	t.Helper()
	prev := now
	now = func() time.Time { return at }
	t.Cleanup(func() { now = prev })
}

// fakeCampaignRepo is an in-memory Repository. It exists to let the
// service's rules be tested without a database; the real queries are tested
// against sqlmock in internal/postgres.
type fakeCampaignRepo struct {
	rows map[string]*Campaign
	// err, when set, is returned by every method — for the "the store failed"
	// paths, which have to stay distinguishable from the domain's own rejections.
	err error
	// deletedEvents is what Delete reports as having cascaded.
	deletedEvents int64
}

func newFakeCampaignRepo(seed ...*Campaign) *fakeCampaignRepo {
	r := &fakeCampaignRepo{rows: map[string]*Campaign{}}
	for _, c := range seed {
		r.rows[c.ID] = c
	}
	return r
}

func (r *fakeCampaignRepo) Create(_ context.Context, c *Campaign) error {
	if r.err != nil {
		return r.err
	}
	copied := *c
	r.rows[c.ID] = &copied
	return nil
}

func (r *fakeCampaignRepo) Get(_ context.Context, id string) (*Campaign, error) {
	if r.err != nil {
		return nil, r.err
	}
	c, ok := r.rows[id]
	if !ok {
		return nil, ErrNotFound
	}
	copied := *c
	return &copied, nil
}

func (r *fakeCampaignRepo) List(_ context.Context, p Page) ([]Campaign, *Cursor, error) {
	if r.err != nil {
		return nil, nil, r.err
	}
	// Enough to observe the clamp: the service is what decides p.Size, and that is
	// the only thing these list tests assert.
	out := make([]Campaign, 0, len(r.rows))
	for _, c := range r.rows {
		out = append(out, *c)
	}
	if len(out) > p.Size {
		out = out[:p.Size]
	}
	return out, nil, nil
}

func (r *fakeCampaignRepo) Update(_ context.Context, c *Campaign) error {
	if r.err != nil {
		return r.err
	}
	if _, ok := r.rows[c.ID]; !ok {
		return ErrNotFound
	}
	copied := *c
	r.rows[c.ID] = &copied
	return nil
}

func (r *fakeCampaignRepo) Delete(_ context.Context, id string) (int64, error) {
	if r.err != nil {
		return 0, r.err
	}
	if _, ok := r.rows[id]; !ok {
		return 0, ErrNotFound
	}
	delete(r.rows, id)
	return r.deletedEvents, nil
}

func (r *fakeCampaignRepo) Exists(_ context.Context, id string) (bool, error) {
	if r.err != nil {
		return false, r.err
	}
	_, ok := r.rows[id]
	return ok, nil
}

// fakeEventRepo is an in-memory EventRepository.
type fakeEventRepo struct {
	rows map[string]*Event
	err  error
	// lastPage records what the service asked for, so the page-size clamp can be
	// asserted on the value that actually reached the store.
	lastPage Page
}

func newFakeEventRepo(seed ...*Event) *fakeEventRepo {
	r := &fakeEventRepo{rows: map[string]*Event{}}
	for _, e := range seed {
		r.rows[e.ID] = e
	}
	return r
}

func (r *fakeEventRepo) Create(_ context.Context, e *Event) error {
	if r.err != nil {
		return r.err
	}
	copied := *e
	r.rows[e.ID] = &copied
	return nil
}

func (r *fakeEventRepo) Get(_ context.Context, id string) (*Event, error) {
	if r.err != nil {
		return nil, r.err
	}
	e, ok := r.rows[id]
	if !ok {
		return nil, ErrNotFound
	}
	copied := *e
	return &copied, nil
}

func (r *fakeEventRepo) ListByCampaign(_ context.Context, campaignID string, p Page) ([]Event, *Cursor, error) {
	r.lastPage = p
	if r.err != nil {
		return nil, nil, r.err
	}
	var out []Event
	for _, e := range r.rows {
		if e.CampaignID == campaignID {
			out = append(out, *e)
		}
	}
	return out, nil, nil
}

func (r *fakeEventRepo) Update(_ context.Context, e *Event) error {
	if r.err != nil {
		return r.err
	}
	if _, ok := r.rows[e.ID]; !ok {
		return ErrNotFound
	}
	copied := *e
	r.rows[e.ID] = &copied
	return nil
}

func (r *fakeEventRepo) Delete(_ context.Context, id string) error {
	if r.err != nil {
		return r.err
	}
	if _, ok := r.rows[id]; !ok {
		return ErrNotFound
	}
	delete(r.rows, id)
	return nil
}

func (r *fakeEventRepo) CountByCampaign(_ context.Context, campaignID string) (int64, error) {
	if r.err != nil {
		return 0, r.err
	}
	var n int64
	for _, e := range r.rows {
		if e.CampaignID == campaignID {
			n++
		}
	}
	return n, nil
}

// newTestService wires the service over the two fakes.
func newTestService(t *testing.T, campaigns *fakeCampaignRepo, events *fakeEventRepo) *Service {
	t.Helper()
	return NewService(&nopLogger, campaigns, events)
}

func TestCreateCampaign(t *testing.T) {
	at := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	later := at.Add(time.Hour)
	earlier := at.Add(-time.Hour)

	tests := []struct {
		name       string
		in         CreateCampaignInput
		wantErr    error
		wantStatus Status
	}{
		{
			name:       "unspecified status defaults to draft",
			in:         CreateCampaignInput{Name: "summer"},
			wantStatus: StatusDraft,
		},
		{
			name:       "explicit status is kept",
			in:         CreateCampaignInput{Name: "summer", Status: StatusActive},
			wantStatus: StatusActive,
		},
		{
			name:    "name is required",
			in:      CreateCampaignInput{},
			wantErr: ErrInvalidArgument,
		},
		{
			name:    "unknown status is rejected",
			in:      CreateCampaignInput{Name: "summer", Status: Status("running")},
			wantErr: ErrInvalidArgument,
		},
		{
			name:    "end before start is rejected",
			in:      CreateCampaignInput{Name: "summer", StartAt: &at, EndAt: &earlier},
			wantErr: ErrInvalidArgument,
		},
		{
			name:       "a window in order is accepted",
			in:         CreateCampaignInput{Name: "summer", StartAt: &at, EndAt: &later},
			wantStatus: StatusDraft,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixedNow(t, at)
			repo := newFakeCampaignRepo()
			svc := newTestService(t, repo, newFakeEventRepo())

			got, err := svc.CreateCampaign(context.Background(), tt.in)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				if len(repo.rows) != 0 {
					t.Errorf("a rejected create stored %d rows, want 0", len(repo.rows))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tt.wantStatus {
				t.Errorf("status = %q, want %q", got.Status, tt.wantStatus)
			}
			if got.ID == "" {
				t.Error("id was not assigned")
			}
			if !got.CreatedAt.Equal(at) || !got.UpdatedAt.Equal(at) {
				t.Errorf("timestamps = %v/%v, want both %v", got.CreatedAt, got.UpdatedAt, at)
			}
			if _, ok := repo.rows[got.ID]; !ok {
				t.Error("the campaign was not stored")
			}
		})
	}
}

func TestUpdateCampaign(t *testing.T) {
	at := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	updatedAt := at.Add(time.Hour)
	start := at.Add(24 * time.Hour)
	end := start.Add(24 * time.Hour)
	beforeStart := start.Add(-time.Hour)

	stored := func() *Campaign {
		return &Campaign{
			ID: campaignID, Name: "summer", Description: "the old one",
			Status: StatusDraft, StartAt: &start, EndAt: &end,
			CreatedAt: at, UpdatedAt: at,
		}
	}
	ptr := func(s string) *string { return &s }
	statusPtr := func(s Status) *Status { return &s }

	tests := []struct {
		name    string
		in      UpdateCampaignInput
		wantErr error
		check   func(t *testing.T, c *Campaign)
	}{
		{
			name: "an absent field is left alone",
			in:   UpdateCampaignInput{ID: campaignID, Status: statusPtr(StatusActive)},
			check: func(t *testing.T, c *Campaign) {
				if c.Name != "summer" || c.Description != "the old one" {
					t.Errorf("untouched fields changed: %+v", c)
				}
				if c.Status != StatusActive {
					t.Errorf("status = %q, want active", c.Status)
				}
			},
		},
		{
			name: "an empty string is a value, not an omission",
			in:   UpdateCampaignInput{ID: campaignID, Description: ptr("")},
			check: func(t *testing.T, c *Campaign) {
				if c.Description != "" {
					t.Errorf("description = %q, want it cleared", c.Description)
				}
			},
		},
		{
			name: "clear beats a supplied value",
			in:   UpdateCampaignInput{ID: campaignID, StartAt: &start, ClearStartAt: true},
			check: func(t *testing.T, c *Campaign) {
				if c.StartAt != nil {
					t.Errorf("start_at = %v, want nil", c.StartAt)
				}
			},
		},
		{
			name: "created_at is not touched and updated_at is",
			in:   UpdateCampaignInput{ID: campaignID, Name: ptr("winter")},
			check: func(t *testing.T, c *Campaign) {
				if !c.CreatedAt.Equal(at) {
					t.Errorf("created_at = %v, want %v", c.CreatedAt, at)
				}
				if !c.UpdatedAt.Equal(updatedAt) {
					t.Errorf("updated_at = %v, want %v", c.UpdatedAt, updatedAt)
				}
			},
		},
		{
			// The reason update is read-then-write: this request names one field,
			// and the rule it breaks is about that field and the stored one.
			name:    "a partial update is validated against the stored row",
			in:      UpdateCampaignInput{ID: campaignID, EndAt: &beforeStart},
			wantErr: ErrInvalidArgument,
		},
		{
			name:    "an unknown campaign is not found",
			in:      UpdateCampaignInput{ID: otherID, Name: ptr("winter")},
			wantErr: ErrNotFound,
		},
		{
			name:    "a malformed id is a bad argument, not a not-found",
			in:      UpdateCampaignInput{ID: "not-a-uuid", Name: ptr("winter")},
			wantErr: ErrInvalidArgument,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixedNow(t, updatedAt)
			repo := newFakeCampaignRepo(stored())
			svc := newTestService(t, repo, newFakeEventRepo())

			got, err := svc.UpdateCampaign(context.Background(), tt.in)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, got)
			// And the same again from the store, so a check that only inspected the
			// returned copy cannot pass while the write was dropped.
			reread, err := repo.Get(context.Background(), campaignID)
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, reread)
		})
	}
}

func TestDeleteCampaignReportsCascade(t *testing.T) {
	repo := newFakeCampaignRepo(&Campaign{ID: campaignID, Name: "summer", Status: StatusDraft})
	repo.deletedEvents = 3
	svc := newTestService(t, repo, newFakeEventRepo())

	got, err := svc.DeleteCampaign(context.Background(), campaignID)
	if err != nil {
		t.Fatal(err)
	}
	if got != 3 {
		t.Errorf("deleted events = %d, want 3", got)
	}
	if _, err := svc.DeleteCampaign(context.Background(), campaignID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete err = %v, want ErrNotFound", err)
	}
}

// TestCreateEventRequiresCampaign is the rule the whole aggregate exists for: an
// event with no campaign is not something this domain can store.
func TestCreateEventRequiresCampaign(t *testing.T) {
	at := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		seed    bool
		in      CreateEventInput
		wantErr error
	}{
		{
			name: "under an existing campaign",
			seed: true,
			in:   CreateEventInput{CampaignID: campaignID, Name: "signup"},
		},
		{
			name:    "under a campaign that does not exist",
			seed:    false,
			in:      CreateEventInput{CampaignID: campaignID, Name: "signup"},
			wantErr: ErrCampaignNotFound,
		},
		{
			name:    "with no campaign at all",
			seed:    true,
			in:      CreateEventInput{Name: "signup"},
			wantErr: ErrInvalidArgument,
		},
		{
			name:    "with a campaign id that is not a uuid",
			seed:    true,
			in:      CreateEventInput{CampaignID: "c1", Name: "signup"},
			wantErr: ErrInvalidArgument,
		},
		{
			name:    "with no name",
			seed:    true,
			in:      CreateEventInput{CampaignID: campaignID},
			wantErr: ErrInvalidArgument,
		},
		{
			name:    "with a payload that is not an object",
			seed:    true,
			in:      CreateEventInput{CampaignID: campaignID, Name: "signup", Payload: json.RawMessage(`[1,2]`)},
			wantErr: ErrInvalidArgument,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixedNow(t, at)
			var seed []*Campaign
			if tt.seed {
				seed = append(seed, &Campaign{ID: campaignID, Name: "summer", Status: StatusActive})
			}
			events := newFakeEventRepo()
			svc := newTestService(t, newFakeCampaignRepo(seed...), events)

			got, err := svc.CreateEvent(context.Background(), tt.in)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				if len(events.rows) != 0 {
					t.Errorf("a rejected create stored %d events, want 0", len(events.rows))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !got.OccurredAt.Equal(at) {
				t.Errorf("occurred_at = %v, want the current time %v", got.OccurredAt, at)
			}
			// The column is NOT NULL, so an absent payload has to become an object
			// rather than a nil that would be written as NULL.
			if string(got.Payload) != `{}` {
				t.Errorf("payload = %q, want {}", got.Payload)
			}
		})
	}
}

func TestCreateEventKeepsSuppliedOccurredAt(t *testing.T) {
	at := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	backfilled := at.Add(-72 * time.Hour)
	fixedNow(t, at)

	svc := newTestService(t,
		newFakeCampaignRepo(&Campaign{ID: campaignID, Name: "summer", Status: StatusActive}),
		newFakeEventRepo())

	got, err := svc.CreateEvent(context.Background(), CreateEventInput{
		CampaignID: campaignID, Name: "signup", OccurredAt: &backfilled,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.OccurredAt.Equal(backfilled) {
		t.Errorf("occurred_at = %v, want the backfilled %v", got.OccurredAt, backfilled)
	}
	if !got.CreatedAt.Equal(at) {
		t.Errorf("created_at = %v, want the write time %v", got.CreatedAt, at)
	}
}

// TestUpdateEventCannotReparent is the negative of the aggregate rule: there is no
// input field for the parent, so an update cannot move an event between campaigns.
func TestUpdateEventCannotReparent(t *testing.T) {
	at := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	fixedNow(t, at.Add(time.Hour))

	events := newFakeEventRepo(&Event{
		ID: eventID, CampaignID: campaignID, Name: "signup",
		Payload: json.RawMessage(`{}`), OccurredAt: at, CreatedAt: at, UpdatedAt: at,
	})
	svc := newTestService(t, newFakeCampaignRepo(), events)

	name := "signup-v2"
	got, err := svc.UpdateEvent(context.Background(), UpdateEventInput{ID: eventID, Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	if got.CampaignID != campaignID {
		t.Errorf("campaign_id = %q, want it unchanged at %q", got.CampaignID, campaignID)
	}
	if got.Name != name {
		t.Errorf("name = %q, want %q", got.Name, name)
	}
	if !got.UpdatedAt.After(at) {
		t.Errorf("updated_at = %v, want it moved past %v", got.UpdatedAt, at)
	}
}

func TestListEventsChecksCampaignAndClampsPageSize(t *testing.T) {
	tests := []struct {
		name     string
		seed     bool
		size     int
		wantSize int
		wantErr  error
	}{
		{name: "unset size becomes the default", seed: true, size: 0, wantSize: DefaultPageSize},
		{name: "a negative size becomes the default", seed: true, size: -5, wantSize: DefaultPageSize},
		{name: "an oversized request is clamped, not rejected", seed: true, size: 5000, wantSize: MaxPageSize},
		{name: "a size in range is passed through", seed: true, size: 7, wantSize: 7},
		{
			// An empty page would be indistinguishable from a campaign with no
			// events yet, so an unknown campaign has to say so.
			name:    "an unknown campaign is reported, not answered with an empty page",
			seed:    false,
			size:    10,
			wantErr: ErrCampaignNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seed []*Campaign
			if tt.seed {
				seed = append(seed, &Campaign{ID: campaignID, Name: "summer", Status: StatusActive})
			}
			events := newFakeEventRepo()
			svc := newTestService(t, newFakeCampaignRepo(seed...), events)

			_, _, err := svc.ListEvents(context.Background(), campaignID, tt.size, "")
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if events.lastPage.Size != tt.wantSize {
				t.Errorf("page size reaching the store = %d, want %d", events.lastPage.Size, tt.wantSize)
			}
		})
	}
}

func TestListRejectsMalformedPageToken(t *testing.T) {
	svc := newTestService(t,
		newFakeCampaignRepo(&Campaign{ID: campaignID, Name: "summer", Status: StatusActive}),
		newFakeEventRepo())

	// Not a silent restart from page 1: a paging loop handed page 1 forever does
	// not terminate.
	if _, _, err := svc.ListCampaigns(context.Background(), 10, "!!!not-base64"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("ListCampaigns err = %v, want ErrInvalidArgument", err)
	}
	if _, _, err := svc.ListEvents(context.Background(), campaignID, 10, "!!!not-base64"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("ListEvents err = %v, want ErrInvalidArgument", err)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		in   Cursor
	}{
		{name: "whole seconds", in: Cursor{Time: time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC), ID: campaignID}},
		{
			// The nanoseconds have to survive, or a cursor rounded to the second
			// skips or repeats every row sharing that second.
			name: "nanosecond precision",
			in:   Cursor{Time: time.Date(2026, 7, 30, 12, 0, 0, 123456789, time.UTC), ID: campaignID},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeCursor(tt.in.Encode())
			if err != nil {
				t.Fatal(err)
			}
			if !got.Time.Equal(tt.in.Time) {
				t.Errorf("time = %v, want %v", got.Time, tt.in.Time)
			}
			if got.ID != tt.in.ID {
				t.Errorf("id = %q, want %q", got.ID, tt.in.ID)
			}
		})
	}
}

func TestDecodeCursor(t *testing.T) {
	tests := []struct {
		name    string
		token   string
		wantNil bool
		wantErr error
	}{
		{name: "empty means the first page", token: "", wantNil: true},
		{name: "not base64", token: "!!!", wantErr: ErrInvalidArgument},
		{name: "no separator", token: encodeRaw("2026-07-30T12:00:00Z"), wantErr: ErrInvalidArgument},
		{name: "empty id", token: encodeRaw("2026-07-30T12:00:00Z|"), wantErr: ErrInvalidArgument},
		{name: "unparseable time", token: encodeRaw("yesterday|" + campaignID), wantErr: ErrInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeCursor(tt.token)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantNil && got != nil {
				t.Errorf("cursor = %+v, want nil", got)
			}
		})
	}
}

func TestStoreFailureIsNotADomainError(t *testing.T) {
	// An error from the store must not be mistaken for one of the domain's own
	// rejections: rpc.go maps those to 4xx codes, and a database outage answered
	// as invalid_argument tells a caller to fix a request that was fine.
	boom := errors.New("connection refused")
	repo := newFakeCampaignRepo()
	repo.err = boom
	svc := newTestService(t, repo, newFakeEventRepo())

	_, err := svc.CreateCampaign(context.Background(), CreateCampaignInput{Name: "summer"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap %v", err, boom)
	}
	for _, sentinel := range []error{ErrInvalidArgument, ErrNotFound, ErrCampaignNotFound} {
		if errors.Is(err, sentinel) {
			t.Errorf("a store failure matched %v", sentinel)
		}
	}
}
