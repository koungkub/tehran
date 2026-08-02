package postgres

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/koungkub/tehran/internal/campaign"
)

// EventRepository is the event store.
type EventRepository struct {
	db *gorm.DB
}

var _ campaign.EventRepository = (*EventRepository)(nil)

// NewEventRepository builds the store over an open GORM handle.
func NewEventRepository(db *gorm.DB) *EventRepository {
	return &EventRepository{db: db}
}

// eventMutableColumns are the columns Update writes. campaign_id is not among
// them: an event's parent is fixed at creation, and leaving the column out means
// no update can move it whatever it was handed.
var eventMutableColumns = []string{"name", "type", "payload", "occurred_at", "updated_at"}

// Create inserts an event.
//
// A foreign key violation becomes ErrCampaignNotFound. The service checks the
// campaign first and this path is only reached when the campaign was deleted
// between that check and this insert, but the caller's answer is the same either
// way, so the two arrive as the same error.
func (r *EventRepository) Create(ctx context.Context, e *campaign.Event) error {
	err := r.db.WithContext(ctx).Create(e).Error
	switch {
	case isForeignKeyViolation(err):
		return fmt.Errorf("%w: %s", campaign.ErrCampaignNotFound, e.CampaignID)
	case err != nil:
		return fmt.Errorf("create event: %w", err)
	}
	return nil
}

// Get returns one event, or campaign.ErrNotFound.
func (r *EventRepository) Get(ctx context.Context, id string) (*campaign.Event, error) {
	var e campaign.Event
	err := r.db.WithContext(ctx).Where("id = ?", id).Take(&e).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, fmt.Errorf("%w: event %s", campaign.ErrNotFound, id)
	case err != nil:
		return nil, fmt.Errorf("get event: %w", err)
	}
	return &e, nil
}

// ListByCampaign walks one campaign's events newest-first by occurred_at.
//
// The predicate is (campaign_id = ?) followed by a keyset on (occurred_at, id),
// which is the column order of events_campaign_occurred_idx — equality first, then
// the range — so the whole page comes off that index.
func (r *EventRepository) ListByCampaign(
	ctx context.Context,
	campaignID string,
	p campaign.Page,
) ([]campaign.Event, *campaign.Cursor, error) {
	q := r.db.WithContext(ctx).
		Where("campaign_id = ?", campaignID).
		Order("occurred_at DESC, id DESC").
		Limit(p.Size + 1)
	if p.Cursor != nil {
		q = q.Where("(occurred_at, id) < (?, ?)", p.Cursor.Time, p.Cursor.ID)
	}

	var rows []campaign.Event
	if err := q.Find(&rows).Error; err != nil {
		return nil, nil, fmt.Errorf("list events: %w", err)
	}
	if len(rows) <= p.Size {
		return rows, nil, nil
	}
	rows = rows[:p.Size]
	last := rows[len(rows)-1]
	// The cursor is on occurred_at, the column this list is ordered by — not on
	// created_at, which the campaign list uses. A cursor built from the wrong
	// column would skip rows silently.
	return rows, &campaign.Cursor{Time: last.OccurredAt, ID: last.ID}, nil
}

// Update writes the mutable columns of e, and reports campaign.ErrNotFound when
// the id matched nothing.
func (r *EventRepository) Update(ctx context.Context, e *campaign.Event) error {
	res := r.db.WithContext(ctx).
		Model(&campaign.Event{}).
		Where("id = ?", e.ID).
		Select(eventMutableColumns).
		Updates(e)
	if res.Error != nil {
		return fmt.Errorf("update event: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: event %s", campaign.ErrNotFound, e.ID)
	}
	return nil
}

// Delete removes one event, leaving its campaign alone.
func (r *EventRepository) Delete(ctx context.Context, id string) error {
	res := r.db.WithContext(ctx).Where("id = ?", id).Delete(&campaign.Event{})
	if res.Error != nil {
		return fmt.Errorf("delete event: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: event %s", campaign.ErrNotFound, id)
	}
	return nil
}

// CountByCampaign counts one campaign's events.
func (r *EventRepository) CountByCampaign(ctx context.Context, campaignID string) (int64, error) {
	var n int64
	if err := r.db.WithContext(ctx).
		Model(&campaign.Event{}).
		Where("campaign_id = ?", campaignID).
		Count(&n).Error; err != nil {
		return 0, fmt.Errorf("count events: %w", err)
	}
	return n, nil
}
