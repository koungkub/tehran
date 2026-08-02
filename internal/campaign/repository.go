package campaign

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

// The repository interfaces are declared here, by the domain that uses them,
// rather than by the package that implements them. That is the direction that
// keeps this package free of any database: internal/postgres imports campaign to
// satisfy these, and campaign imports nothing to define them. .golangci.yml
// enforces the direction with a depguard rule.

// Repository is persistence for the aggregate root. Named for the package rather
// than for Campaign — campaign.Repository, not campaign.CampaignRepository — since
// the aggregate root is what the context is about.
type Repository interface {
	Create(ctx context.Context, c *Campaign) error
	// Get returns ErrNotFound when there is no such campaign.
	Get(ctx context.Context, id string) (*Campaign, error)
	// List returns up to p.Size campaigns newest-first, plus the cursor for the
	// next page — nil when this was the last one.
	List(ctx context.Context, p Page) ([]Campaign, *Cursor, error)
	// Update writes every mutable column of c and returns ErrNotFound when the id
	// matched nothing. The service has already merged the caller's partial update
	// onto the stored row, so a full-row write is what arrives here.
	Update(ctx context.Context, c *Campaign) error
	// Delete removes the campaign and, through the foreign key's cascade, its
	// events. It returns how many events went with it, and ErrNotFound when the id
	// matched nothing.
	Delete(ctx context.Context, id string) (deletedEvents int64, err error)
	// Exists is the primitive behind "every event must have a campaign": cheaper
	// than Get, which would fetch a row nobody reads.
	Exists(ctx context.Context, id string) (bool, error)
}

// EventRepository is persistence for the child entity.
type EventRepository interface {
	// Create returns ErrCampaignNotFound when the foreign key rejects the insert,
	// which is the race the service's own Exists check cannot close.
	Create(ctx context.Context, e *Event) error
	Get(ctx context.Context, id string) (*Event, error)
	// ListByCampaign returns up to p.Size events of one campaign, newest-first by
	// occurred_at. There is no List without a campaign: see EventService in the
	// proto for why.
	ListByCampaign(ctx context.Context, campaignID string, p Page) ([]Event, *Cursor, error)
	Update(ctx context.Context, e *Event) error
	Delete(ctx context.Context, id string) error
	// CountByCampaign is used by tests and by nothing in the request path.
	CountByCampaign(ctx context.Context, campaignID string) (int64, error)
}

// Pagination bounds. DefaultPageSize is what an unset page_size becomes;
// MaxPageSize is the cap a larger one is clamped to rather than rejected, so a
// client asking for 1000 gets a page instead of an error.
const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// Page is one page request: a size, and where to resume from.
//
// Keyset, not offset. An offset makes the database walk and discard every row
// before the page — cost grows with the page number — and it shifts under
// concurrent inserts, so a row inserted while a client pages through is seen
// twice or not at all. A cursor on the sort key has neither problem.
type Page struct {
	// Size is the number of rows wanted. Clamp it with ClampPageSize before
	// building a Page; the repositories trust it.
	Size int
	// Cursor is nil on the first page, and otherwise the last row of the previous
	// one.
	Cursor *Cursor
}

// Cursor identifies the last row of a page by its sort key.
//
// Both fields, because Time alone is not unique: two campaigns created in the
// same microsecond would make the boundary ambiguous, and a cursor on the pair
// (which is unique, since ID is) cannot skip or repeat a row.
type Cursor struct {
	Time time.Time
	ID   string
}

// cursorSeparator cannot appear in either half: RFC3339Nano has no pipe and a
// UUID has no pipe, so splitting on it is unambiguous.
const cursorSeparator = "|"

// Encode renders the cursor as the opaque page token the API hands out. Opaque
// is the point — base64 is not obfuscation, it is a signal that the contents are
// this service's business and the shape can change without breaking a client
// that only ever passes the token back.
func (c Cursor) Encode() string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(c.Time.UTC().Format(time.RFC3339Nano) + cursorSeparator + c.ID),
	)
}

// DecodeCursor parses a page token. An empty token means "first page" and yields
// a nil cursor with no error; anything unparseable is ErrInvalidArgument.
//
// Rejecting a bad token matters: silently starting from the top instead would
// answer a client that has drifted with page 1, and a paging loop that keeps
// being handed page 1 does not terminate.
func DecodeCursor(token string) (*Cursor, error) {
	if token == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed page_token", ErrInvalidArgument)
	}
	ts, id, ok := strings.Cut(string(raw), cursorSeparator)
	if !ok || id == "" {
		return nil, fmt.Errorf("%w: malformed page_token", ErrInvalidArgument)
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed page_token", ErrInvalidArgument)
	}
	return &Cursor{Time: t, ID: id}, nil
}

// ClampPageSize turns a requested size into a usable one: unset or negative
// becomes the default, oversized becomes the maximum.
func ClampPageSize(size int) int {
	switch {
	case size <= 0:
		return DefaultPageSize
	case size > MaxPageSize:
		return MaxPageSize
	}
	return size
}
