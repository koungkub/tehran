package campaign

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// emptyPayload is what an event with no payload is stored as. The column is
// NOT NULL with this as its default, but GORM writes every column it knows about,
// so a nil here would be sent as an explicit NULL and rejected — the default only
// applies to a statement that omits the column.
var emptyPayload = json.RawMessage(`{}`)

// Service is the campaign domain logic. It knows nothing about transports:
// adapters translate to and from it, which is what lets the same rules serve
// ConnectRPC today and a queue consumer later.
type Service struct {
	log       *zerolog.Logger
	campaigns Repository
	events    EventRepository
}

// NewService builds the domain service over its two repositories.
//
// Unlike greeter, this domain has no meaningful behaviour without persistence, so
// both repositories are required rather than optional. The composition root
// refuses to start the api command with the database section disabled; passing
// nil here would only move the failure to the first request.
func NewService(log *zerolog.Logger, campaigns Repository, events EventRepository) *Service {
	return &Service{log: log, campaigns: campaigns, events: events}
}

// now is the single clock the service stamps rows from, held on the package rather
// than called inline so that a test can pin it.
var now = func() time.Time { return time.Now().UTC() }

// CreateCampaignInput is a create request, already free of any wire format.
type CreateCampaignInput struct {
	Name        string
	Description string
	// Status is optional: empty becomes StatusDraft, since a campaign that is
	// created is not yet running.
	Status  Status
	StartAt *time.Time
	EndAt   *time.Time
}

// CreateCampaign validates the input, assigns an id and timestamps, and stores it.
func (s *Service) CreateCampaign(ctx context.Context, in CreateCampaignInput) (*Campaign, error) {
	status := in.Status
	if status == "" {
		status = StatusDraft
	}
	ts := now()
	c := &Campaign{
		ID:          uuid.NewString(),
		Name:        in.Name,
		Description: in.Description,
		Status:      status,
		StartAt:     utcOrNil(in.StartAt),
		EndAt:       utcOrNil(in.EndAt),
		CreatedAt:   ts,
		UpdatedAt:   ts,
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if err := s.campaigns.Create(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

// GetCampaign returns one campaign, or ErrNotFound.
func (s *Service) GetCampaign(ctx context.Context, id string) (*Campaign, error) {
	if err := validateID(id, "id"); err != nil {
		return nil, err
	}
	return s.campaigns.Get(ctx, id)
}

// ListCampaigns returns a page of campaigns newest-first and the token for the
// next page, empty when this was the last.
func (s *Service) ListCampaigns(ctx context.Context, pageSize int, pageToken string) ([]Campaign, string, error) {
	cursor, err := DecodeCursor(pageToken)
	if err != nil {
		return nil, "", err
	}
	items, next, err := s.campaigns.List(ctx, Page{Size: ClampPageSize(pageSize), Cursor: cursor})
	if err != nil {
		return nil, "", err
	}
	return items, encodeNext(next), nil
}

// UpdateCampaignInput is a partial update. A nil pointer field is left alone; the
// Clear flags are how a timestamp is set back to unset, which a nil pointer
// cannot express because it already means "no change".
type UpdateCampaignInput struct {
	ID           string
	Name         *string
	Description  *string
	Status       *Status
	StartAt      *time.Time
	EndAt        *time.Time
	ClearStartAt bool
	ClearEndAt   bool
}

// UpdateCampaign reads the stored row, merges the fields the caller supplied onto
// it, validates the result and writes it back.
//
// Read-then-write rather than a targeted UPDATE: validating a partial update
// otherwise means validating fields in isolation, and campaigns_window_check is a
// rule about two of them together — a request that moves start_at alone can only
// be checked against the end_at already stored.
func (s *Service) UpdateCampaign(ctx context.Context, in UpdateCampaignInput) (*Campaign, error) {
	if err := validateID(in.ID, "id"); err != nil {
		return nil, err
	}
	c, err := s.campaigns.Get(ctx, in.ID)
	if err != nil {
		return nil, err
	}

	if in.Name != nil {
		c.Name = *in.Name
	}
	if in.Description != nil {
		c.Description = *in.Description
	}
	if in.Status != nil {
		c.Status = *in.Status
	}
	// Clear wins over a value: a request carrying both contradicts itself, and
	// dropping the timestamp is the safer reading of it.
	switch {
	case in.ClearStartAt:
		c.StartAt = nil
	case in.StartAt != nil:
		c.StartAt = utcOrNil(in.StartAt)
	}
	switch {
	case in.ClearEndAt:
		c.EndAt = nil
	case in.EndAt != nil:
		c.EndAt = utcOrNil(in.EndAt)
	}
	c.UpdatedAt = now()

	if err := c.Validate(); err != nil {
		return nil, err
	}
	if err := s.campaigns.Update(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

// DeleteCampaign removes a campaign and every event under it, reporting how many
// events went with it.
func (s *Service) DeleteCampaign(ctx context.Context, id string) (int64, error) {
	if err := validateID(id, "id"); err != nil {
		return 0, err
	}
	deletedEvents, err := s.campaigns.Delete(ctx, id)
	if err != nil {
		return 0, err
	}
	// Logged at info, not debug: the cascade destroys rows the caller did not name,
	// and after the fact this line is the only record of how many.
	s.log.Info().Ctx(ctx).
		Str("campaign_id", id).
		Int64("deleted_events", deletedEvents).
		Msg("campaign deleted, events cascaded")
	return deletedEvents, nil
}

// CreateEventInput is a create request for an event.
type CreateEventInput struct {
	// CampaignID is required and has to name a campaign that exists.
	CampaignID string
	Name       string
	Type       string
	Payload    json.RawMessage
	// OccurredAt is optional: unset means now, which is right for an event
	// reported as it happens and wrong for a backfill, so a backfill sets it.
	OccurredAt *time.Time
}

// CreateEvent stores an event under an existing campaign.
//
// The parent is checked here, and the foreign key checks it again. Neither is
// redundant: this check is what turns "no such campaign" into an answer the caller
// can act on instead of a driver error, and the constraint is what holds when the
// campaign is deleted between the two statements.
func (s *Service) CreateEvent(ctx context.Context, in CreateEventInput) (*Event, error) {
	if err := validateID(in.CampaignID, "campaign_id"); err != nil {
		return nil, err
	}
	exists, err := s.campaigns.Exists(ctx, in.CampaignID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("%w: %s", ErrCampaignNotFound, in.CampaignID)
	}

	ts := now()
	occurred := ts
	if in.OccurredAt != nil {
		occurred = in.OccurredAt.UTC()
	}
	e := &Event{
		ID:         uuid.NewString(),
		CampaignID: in.CampaignID,
		Name:       in.Name,
		Type:       in.Type,
		Payload:    payloadOrEmpty(in.Payload),
		OccurredAt: occurred,
		CreatedAt:  ts,
		UpdatedAt:  ts,
	}
	if err := e.Validate(); err != nil {
		return nil, err
	}
	if err := s.events.Create(ctx, e); err != nil {
		return nil, err
	}
	return e, nil
}

// GetEvent returns one event, or ErrNotFound.
func (s *Service) GetEvent(ctx context.Context, id string) (*Event, error) {
	if err := validateID(id, "id"); err != nil {
		return nil, err
	}
	return s.events.Get(ctx, id)
}

// ListEvents returns a page of one campaign's events, newest-first by occurred_at.
//
// The campaign is checked first so that an unknown id is reported as such rather
// than as an empty page, which a caller cannot tell from a campaign that simply
// has no events yet.
func (s *Service) ListEvents(ctx context.Context, campaignID string, pageSize int, pageToken string) ([]Event, string, error) {
	if err := validateID(campaignID, "campaign_id"); err != nil {
		return nil, "", err
	}
	cursor, err := DecodeCursor(pageToken)
	if err != nil {
		return nil, "", err
	}
	exists, err := s.campaigns.Exists(ctx, campaignID)
	if err != nil {
		return nil, "", err
	}
	if !exists {
		return nil, "", fmt.Errorf("%w: %s", ErrCampaignNotFound, campaignID)
	}

	items, next, err := s.events.ListByCampaign(ctx, campaignID, Page{
		Size:   ClampPageSize(pageSize),
		Cursor: cursor,
	})
	if err != nil {
		return nil, "", err
	}
	return items, encodeNext(next), nil
}

// UpdateEventInput is a partial update. There is no CampaignID field: an event's
// parent is fixed at creation. See UpdateEvent.
type UpdateEventInput struct {
	ID         string
	Name       *string
	Type       *string
	Payload    json.RawMessage
	OccurredAt *time.Time
}

// UpdateEvent merges the supplied fields onto the stored event.
//
// Reparenting is not an update. Allowing it would let one request move an event
// between campaigns, which is a change to two aggregates' contents disguised as
// an edit to one row — delete and re-create instead, where both effects are
// visible. Enforced in three places, none of which is a runtime check: the proto
// has no campaign_id in UpdateEventRequest, this input type has no field for it,
// and the repository's update column list leaves the column out.
func (s *Service) UpdateEvent(ctx context.Context, in UpdateEventInput) (*Event, error) {
	if err := validateID(in.ID, "id"); err != nil {
		return nil, err
	}
	e, err := s.events.Get(ctx, in.ID)
	if err != nil {
		return nil, err
	}

	if in.Name != nil {
		e.Name = *in.Name
	}
	if in.Type != nil {
		e.Type = *in.Type
	}
	if in.Payload != nil {
		e.Payload = payloadOrEmpty(in.Payload)
	}
	if in.OccurredAt != nil {
		e.OccurredAt = in.OccurredAt.UTC()
	}
	e.UpdatedAt = now()

	if err := e.Validate(); err != nil {
		return nil, err
	}
	if err := s.events.Update(ctx, e); err != nil {
		return nil, err
	}
	return e, nil
}

// DeleteEvent removes one event. Its campaign is untouched.
func (s *Service) DeleteEvent(ctx context.Context, id string) error {
	if err := validateID(id, "id"); err != nil {
		return err
	}
	return s.events.Delete(ctx, id)
}

// validateID rejects an empty or malformed identifier before it reaches the
// database. Both columns are uuid, and a non-uuid string sent to one comes back as
// a driver syntax error — an internal error to the caller, for what is plainly a
// bad argument.
func validateID(id, field string) error {
	if id == "" {
		return fmt.Errorf("%w: %s is required", ErrInvalidArgument, field)
	}
	if _, err := uuid.Parse(id); err != nil {
		return fmt.Errorf("%w: %s is not a uuid", ErrInvalidArgument, field)
	}
	return nil
}

// utcOrNil normalises an optional timestamp, keeping nil as nil. Everything
// stored is UTC, so that a row does not carry the timezone of whichever machine
// happened to write it.
func utcOrNil(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// payloadOrEmpty substitutes the empty JSON object for an absent payload; see
// emptyPayload for why nil cannot be sent.
func payloadOrEmpty(p json.RawMessage) json.RawMessage {
	if len(p) == 0 {
		return emptyPayload
	}
	return p
}

// encodeNext turns the repository's next-page cursor into a token, with the empty
// string standing for "no further pages".
func encodeNext(c *Cursor) string {
	if c == nil {
		return ""
	}
	return c.Encode()
}
