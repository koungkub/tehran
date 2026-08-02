package campaign

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	campaignv1 "github.com/koungkub/tehran/gen/proto/campaign/v1"
	"github.com/koungkub/tehran/gen/proto/campaign/v1/campaignv1connect"
)

// TestToConnectError is the table the whole error contract rests on: every domain
// error has one code, decided here and nowhere else.
func TestToConnectError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want connect.Code
	}{
		{name: "invalid argument", err: fmt.Errorf("%w: name", ErrInvalidArgument), want: connect.CodeInvalidArgument},
		{name: "not found", err: fmt.Errorf("%w: campaign x", ErrNotFound), want: connect.CodeNotFound},
		{
			// On a read path. CreateEvent overrides this before calling in — see
			// TestCreateEventMissingCampaignIsFailedPrecondition.
			name: "missing campaign on a read",
			err:  fmt.Errorf("%w: x", ErrCampaignNotFound),
			want: connect.CodeNotFound,
		},
		{name: "client hung up", err: context.Canceled, want: connect.CodeCanceled},
		{name: "deadline", err: context.DeadlineExceeded, want: connect.CodeDeadlineExceeded},
		{
			// Anything unrecognised, which is the case that must not leak.
			name: "unknown",
			err:  errors.New(`pq: duplicate key value violates unique constraint "campaigns_pkey"`),
			want: connect.CodeInternal,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toConnectError(context.Background(), &nopLogger, tt.err)
			if code := connect.CodeOf(got); code != tt.want {
				t.Errorf("code = %v, want %v", code, tt.want)
			}
			if tt.want == connect.CodeInternal {
				// The reply carries a fixed message: a statement, a column name or
				// the row that collided are all things the caller must not be told.
				if msg := connect.CodeOf(got).String(); msg == "" {
					t.Fatal("no code")
				}
				var connErr *connect.Error
				if !errors.As(got, &connErr) {
					t.Fatalf("err = %v, want a *connect.Error", got)
				}
				if connErr.Message() != "internal error" {
					t.Errorf("message = %q, want %q", connErr.Message(), "internal error")
				}
			}
		})
	}
}

// TestCreateEventMissingCampaignIsFailedPrecondition covers the one place a
// sentinel maps to two codes depending on the operation: on a create, the
// collection the caller addressed exists and it is the precondition that fails.
func TestCreateEventMissingCampaignIsFailedPrecondition(t *testing.T) {
	svc := newTestService(t, newFakeCampaignRepo(), newFakeEventRepo())
	h := NewEventHandler(svc)

	_, err := h.CreateEvent(context.Background(), connect.NewRequest(&campaignv1.CreateEventRequest{
		CampaignId: campaignID,
		Name:       "signup",
	}))
	if got, want := connect.CodeOf(err), connect.CodeFailedPrecondition; got != want {
		t.Fatalf("code = %v, want %v", got, want)
	}

	// The same sentinel on a read path is a not-found.
	_, listErr := h.ListEvents(context.Background(), connect.NewRequest(&campaignv1.ListEventsRequest{
		CampaignId: campaignID,
	}))
	if got, want := connect.CodeOf(listErr), connect.CodeNotFound; got != want {
		t.Errorf("list code = %v, want %v", got, want)
	}
}

func TestStatusProtoRoundTrip(t *testing.T) {
	all := []Status{StatusDraft, StatusActive, StatusPaused, StatusEnded}
	for _, s := range all {
		t.Run(string(s), func(t *testing.T) {
			if got := statusFromProto(statusToProto(s)); got != s {
				t.Errorf("round trip = %q, want %q", got, s)
			}
		})
	}

	t.Run("unspecified is the empty status", func(t *testing.T) {
		if got := statusFromProto(campaignv1.CampaignStatus_CAMPAIGN_STATUS_UNSPECIFIED); got != "" {
			t.Errorf("status = %q, want empty", got)
		}
	})
	t.Run("an unknown domain status is unspecified", func(t *testing.T) {
		got := statusToProto(Status("running"))
		if got != campaignv1.CampaignStatus_CAMPAIGN_STATUS_UNSPECIFIED {
			t.Errorf("status = %v, want unspecified", got)
		}
	})
}

func TestCampaignToProto(t *testing.T) {
	at := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	start := at.Add(24 * time.Hour)

	t.Run("an unset window stays unset rather than becoming the epoch", func(t *testing.T) {
		got := campaignToProto(&Campaign{
			ID: campaignID, Name: "summer", Status: StatusDraft,
			CreatedAt: at, UpdatedAt: at,
		})
		if got.GetStartAt() != nil || got.GetEndAt() != nil {
			t.Errorf("window = %v/%v, want both nil", got.GetStartAt(), got.GetEndAt())
		}
		if !got.GetCreatedAt().AsTime().Equal(at) {
			t.Errorf("created_at = %v, want %v", got.GetCreatedAt().AsTime(), at)
		}
	})

	t.Run("a set window is carried", func(t *testing.T) {
		got := campaignToProto(&Campaign{
			ID: campaignID, Name: "summer", Status: StatusActive,
			StartAt: &start, CreatedAt: at, UpdatedAt: at,
		})
		if !got.GetStartAt().AsTime().Equal(start) {
			t.Errorf("start_at = %v, want %v", got.GetStartAt().AsTime(), start)
		}
		if got.GetEndAt() != nil {
			t.Errorf("end_at = %v, want nil", got.GetEndAt())
		}
	})
}

func TestEventPayloadConversion(t *testing.T) {
	t.Run("a nil struct is no payload, not an empty one", func(t *testing.T) {
		got, err := payloadFromProto(nil)
		if err != nil {
			t.Fatal(err)
		}
		if got != nil {
			t.Errorf("payload = %q, want nil", got)
		}
	})

	t.Run("empty stored bytes come back as nil", func(t *testing.T) {
		got, err := payloadToProto(nil)
		if err != nil {
			t.Fatal(err)
		}
		if got != nil {
			t.Errorf("payload = %v, want nil", got)
		}
	})

	t.Run("a document survives the round trip", func(t *testing.T) {
		in, err := structpb.NewStruct(map[string]any{
			"amount":   float64(10),
			"currency": "THB",
			"nested":   map[string]any{"ok": true},
		})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := payloadFromProto(in)
		if err != nil {
			t.Fatal(err)
		}
		out, err := payloadToProto(raw)
		if err != nil {
			t.Fatal(err)
		}
		if in.String() != out.String() {
			t.Errorf("round trip = %v, want %v", out, in)
		}
	})

	t.Run("a stored document that is not an object is a bad argument", func(t *testing.T) {
		// Only reachable from a writer other than this service — Validate refuses
		// it on the way in — but it must not become an internal error.
		_, err := payloadToProto(json.RawMessage(`[1,2,3]`))
		if !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("err = %v, want ErrInvalidArgument", err)
		}
	})
}

func TestTimeFromProto(t *testing.T) {
	at := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		in      *timestamppb.Timestamp
		wantNil bool
	}{
		{name: "nil is unset", in: nil, wantNil: true},
		{
			// timestamppb carries seconds and nanos that need not be in range, and
			// AsTime on an out-of-range value returns a date no column will hold.
			name:    "an out-of-range timestamp is treated as unset",
			in:      &timestamppb.Timestamp{Seconds: -100000000000},
			wantNil: true,
		},
		{name: "a valid timestamp is read", in: timestamppb.New(at)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := timeFromProto(tt.in)
			if tt.wantNil {
				if got != nil {
					t.Errorf("time = %v, want nil", got)
				}
				return
			}
			if got == nil || !got.Equal(at) {
				t.Errorf("time = %v, want %v", got, at)
			}
		})
	}
}

// TestUpdateCampaignStatusPresence is why UpdateCampaignRequest.status is
// `optional`: an absent field and an explicit UNSPECIFIED have to be different
// requests, and only the presence bit can tell them apart.
func TestUpdateCampaignStatusPresence(t *testing.T) {
	at := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	repo := newFakeCampaignRepo(&Campaign{
		ID: campaignID, Name: "summer", Status: StatusActive, CreatedAt: at, UpdatedAt: at,
	})
	h := NewHandler(newTestService(t, repo, newFakeEventRepo()))
	name := "winter"

	// Absent: the stored status is left alone.
	res, err := h.UpdateCampaign(context.Background(), connect.NewRequest(&campaignv1.UpdateCampaignRequest{
		Id:   campaignID,
		Name: &name,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Msg.GetCampaign().GetStatus(); got != campaignv1.CampaignStatus_CAMPAIGN_STATUS_ACTIVE {
		t.Errorf("status = %v, want it unchanged at active", got)
	}

	// Present and unspecified: a status the domain does not accept.
	unspecified := campaignv1.CampaignStatus_CAMPAIGN_STATUS_UNSPECIFIED
	_, err = h.UpdateCampaign(context.Background(), connect.NewRequest(&campaignv1.UpdateCampaignRequest{
		Id:     campaignID,
		Status: &unspecified,
	}))
	if got, want := connect.CodeOf(err), connect.CodeInvalidArgument; got != want {
		t.Errorf("code = %v, want %v", got, want)
	}
}

// TestRegister checks both handlers satisfy connectrpc.Module's contract: they
// mount themselves and report the service names they serve, which is what the
// server's reflection and health endpoints are built from.
func TestRegister(t *testing.T) {
	svc := newTestService(t, newFakeCampaignRepo(), newFakeEventRepo())

	tests := []struct {
		name   string
		module interface {
			Register(*http.ServeMux, ...connect.HandlerOption) []string
		}
		want string
	}{
		{name: "campaigns", module: NewHandler(svc), want: campaignv1connect.CampaignServiceName},
		{name: "events", module: NewEventHandler(svc), want: campaignv1connect.EventServiceName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.module.Register(http.NewServeMux())
			if len(got) != 1 || got[0] != tt.want {
				t.Errorf("service names = %v, want [%s]", got, tt.want)
			}
		})
	}
}

// TestCreateEventThroughHandler walks one request the whole way down to the fake
// store and back out as proto, which is what the conversion helpers are for.
func TestCreateEventThroughHandler(t *testing.T) {
	at := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	fixedNow(t, at)

	events := newFakeEventRepo()
	h := NewEventHandler(newTestService(t,
		newFakeCampaignRepo(&Campaign{ID: campaignID, Name: "summer", Status: StatusActive}),
		events))

	payload, err := structpb.NewStruct(map[string]any{"amount": float64(10)})
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.CreateEvent(context.Background(), connect.NewRequest(&campaignv1.CreateEventRequest{
		CampaignId: campaignID,
		Name:       "signup",
		Type:       "conversion",
		Payload:    payload,
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := res.Msg.GetEvent()
	if got.GetCampaignId() != campaignID {
		t.Errorf("campaign_id = %q, want %q", got.GetCampaignId(), campaignID)
	}
	if !got.GetOccurredAt().AsTime().Equal(at) {
		t.Errorf("occurred_at = %v, want %v", got.GetOccurredAt().AsTime(), at)
	}
	if got.GetPayload().GetFields()["amount"].GetNumberValue() != 10 {
		t.Errorf("payload = %v, want amount 10", got.GetPayload())
	}
	if len(events.rows) != 1 {
		t.Errorf("stored %d events, want 1", len(events.rows))
	}
}
