// Package campaign is the campaign bounded context: campaigns and the events
// that belong to them.
//
// One package for both, deliberately. An event without a campaign is not a thing
// this domain can represent, so every event write has to consult the campaign
// side — across two packages that would either be an import between sibling
// domains (which .golangci.yml denies) or an interface at the composition root
// standing in for a rule that is not optional. Campaign is the aggregate root and
// Event is a child entity of the same aggregate; they are one context.
//
// The files split by layer, following greeter:
//   - model.go holds the entities, their invariants and the error sentinels.
//   - repository.go declares what the domain needs from persistence, as
//     interfaces. It names no database.
//   - service.go is the transport-agnostic domain logic.
//   - rpc.go adapts the Service to the generated ConnectRPC interfaces, and is
//     the only file here that imports connect or gen/.
package campaign

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The errors the layers above match on. The service returns these (wrapped, with
// the specifics in the message); rpc.go maps them to Connect codes and is the
// only place that decides which code each one becomes.
var (
	// ErrNotFound is "no row with that id".
	ErrNotFound = errors.New("not found")
	// ErrInvalidArgument is a request the domain rejects before touching the
	// database: a missing name, an unknown status, a window that ends before it
	// starts.
	ErrInvalidArgument = errors.New("invalid argument")
	// ErrCampaignNotFound is distinct from ErrNotFound on purpose. On an event
	// write it means "the parent is missing", which is a different answer to the
	// caller than "the event is missing" — and a different Connect code.
	ErrCampaignNotFound = errors.New("campaign not found")
)

// Status is a campaign's lifecycle state. The values are the strings stored in
// the column, and they are checked in the database too — see the
// campaigns_status_check constraint, which has to be kept in step with this set.
type Status string

// The four states. A campaign is created as draft, runs while active, can be
// paused and resumed, and ends once. Nothing here enforces an order between them:
// which transitions are legal is a policy this domain does not yet have.
const (
	StatusDraft  Status = "draft"
	StatusActive Status = "active"
	StatusPaused Status = "paused"
	StatusEnded  Status = "ended"
)

// Valid reports whether s is one of the four states.
func (s Status) Valid() bool {
	switch s {
	case StatusDraft, StatusActive, StatusPaused, StatusEnded:
		return true
	}
	return false
}

// Campaign is the aggregate root.
//
// The gorm tags are here rather than on a separate row struct in the persistence
// adapter: one mapping (domain to proto, in rpc.go) instead of two, for a domain
// whose entities are the same shape in both places. The cost is that the entity
// names its storage. If that becomes a problem — a second store, a column layout
// that stops matching the domain — the move is a row type in internal/postgres
// with the tags on it, and nothing outside that package changes.
type Campaign struct {
	ID          string     `gorm:"column:id;primaryKey"`
	Name        string     `gorm:"column:name"`
	Description string     `gorm:"column:description"`
	Status      Status     `gorm:"column:status"`
	StartAt     *time.Time `gorm:"column:start_at"`
	EndAt       *time.Time `gorm:"column:end_at"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
	UpdatedAt   time.Time  `gorm:"column:updated_at"`
}

// TableName pins the table. GORM would infer "campaigns" from the type name here,
// but inference is a rename away from silently pointing at a table that does not
// exist, and the migration spells the name out too.
func (Campaign) TableName() string { return "campaigns" }

// Validate checks the invariants the database also checks. Errors wrap
// ErrInvalidArgument so a caller matches on the class, not the wording.
func (c *Campaign) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidArgument)
	}
	if !c.Status.Valid() {
		return fmt.Errorf("%w: unknown status %q", ErrInvalidArgument, c.Status)
	}
	// Only when both are set: either one alone leaves the window open at that end.
	if c.StartAt != nil && c.EndAt != nil && !c.EndAt.After(*c.StartAt) {
		return fmt.Errorf("%w: end_at must be after start_at", ErrInvalidArgument)
	}
	return nil
}

// Event is a child entity of the Campaign aggregate. It cannot exist without a
// campaign: CampaignID is required here, the column is NOT NULL, and the foreign
// key cascades so the row goes when its campaign does.
type Event struct {
	ID         string `gorm:"column:id;primaryKey"`
	CampaignID string `gorm:"column:campaign_id"`
	Name       string `gorm:"column:name"`
	Type       string `gorm:"column:type"`
	// Payload is stored as jsonb. json.RawMessage rather than a map: the domain
	// does not read into it, so decoding and re-encoding it on every round trip
	// would only be a chance to change it.
	Payload    json.RawMessage `gorm:"column:payload;type:jsonb"`
	OccurredAt time.Time       `gorm:"column:occurred_at"`
	CreatedAt  time.Time       `gorm:"column:created_at"`
	UpdatedAt  time.Time       `gorm:"column:updated_at"`
}

// TableName pins the table, as on Campaign.
func (Event) TableName() string { return "events" }

// Validate checks the invariants. The payload is checked for being valid JSON
// here rather than left to the database, because a jsonb insert of a malformed
// document fails as a driver error with no useful class to match on.
func (e *Event) Validate() error {
	if e.CampaignID == "" {
		return fmt.Errorf("%w: campaign_id is required", ErrInvalidArgument)
	}
	if e.Name == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidArgument)
	}
	if e.OccurredAt.IsZero() {
		return fmt.Errorf("%w: occurred_at is required", ErrInvalidArgument)
	}
	if err := validatePayload(e.Payload); err != nil {
		return err
	}
	return nil
}

// validatePayload requires a JSON object, not merely valid JSON. jsonb accepts a
// bare array or number too, but the wire type is a protobuf Struct, which holds
// only an object — a row storing anything else could not be read back out, so it
// is refused on the way in.
func validatePayload(p json.RawMessage) error {
	if len(p) == 0 {
		return nil
	}
	if !json.Valid(p) {
		return fmt.Errorf("%w: payload is not valid JSON", ErrInvalidArgument)
	}
	if trimmed := bytes.TrimLeft(p, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("%w: payload must be a JSON object", ErrInvalidArgument)
	}
	return nil
}
