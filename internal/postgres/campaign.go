package postgres

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/koungkub/tehran/internal/campaign"
)

// CampaignRepository is the campaign store.
type CampaignRepository struct {
	db *gorm.DB
}

var _ campaign.Repository = (*CampaignRepository)(nil)

// NewCampaignRepository builds the store over an open GORM handle. The handle is
// shared with every other repository: it is the pool, and one pool per repository
// would multiply the service's connection budget by the number of domains.
func NewCampaignRepository(db *gorm.DB) *CampaignRepository {
	return &CampaignRepository{db: db}
}

// campaignMutableColumns are the columns Update writes. Named explicitly because
// GORM skips zero-valued struct fields otherwise, and "" and NULL are legitimate
// values here — a description cleared back to empty has to reach the table.
var campaignMutableColumns = []string{"name", "description", "status", "start_at", "end_at", "updated_at"}

// Create inserts a campaign. The id and the timestamps are assigned by the
// service, so this is a plain insert with no returning clause to read back.
func (r *CampaignRepository) Create(ctx context.Context, c *campaign.Campaign) error {
	if err := r.db.WithContext(ctx).Create(c).Error; err != nil {
		return fmt.Errorf("create campaign: %w", err)
	}
	return nil
}

// Get returns one campaign, or campaign.ErrNotFound.
func (r *CampaignRepository) Get(ctx context.Context, id string) (*campaign.Campaign, error) {
	var c campaign.Campaign
	err := r.db.WithContext(ctx).Where("id = ?", id).Take(&c).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		// Translated at the boundary: the layers above match on the domain's own
		// sentinel and never learn that GORM is behind this.
		return nil, fmt.Errorf("%w: campaign %s", campaign.ErrNotFound, id)
	case err != nil:
		return nil, fmt.Errorf("get campaign: %w", err)
	}
	return &c, nil
}

// List walks campaigns newest-first from the cursor.
func (r *CampaignRepository) List(ctx context.Context, p campaign.Page) ([]campaign.Campaign, *campaign.Cursor, error) {
	// One more row than asked for: its presence is what says there is a next page,
	// and it is dropped before returning. A separate COUNT would be a second scan
	// to answer a question this row already answers.
	q := r.db.WithContext(ctx).
		Order("created_at DESC, id DESC").
		Limit(p.Size + 1)
	if p.Cursor != nil {
		// A row-value comparison, not "created_at < ? OR (created_at = ? AND
		// id < ?)": both are correct, but PostgreSQL matches the first against the
		// (created_at DESC, id DESC) index directly.
		q = q.Where("(created_at, id) < (?, ?)", p.Cursor.Time, p.Cursor.ID)
	}

	var rows []campaign.Campaign
	if err := q.Find(&rows).Error; err != nil {
		return nil, nil, fmt.Errorf("list campaigns: %w", err)
	}
	if len(rows) <= p.Size {
		return rows, nil, nil
	}
	rows = rows[:p.Size]
	last := rows[len(rows)-1]
	return rows, &campaign.Cursor{Time: last.CreatedAt, ID: last.ID}, nil
}

// Update writes the mutable columns of c, and reports campaign.ErrNotFound when
// the id matched nothing.
func (r *CampaignRepository) Update(ctx context.Context, c *campaign.Campaign) error {
	res := r.db.WithContext(ctx).
		Model(&campaign.Campaign{}).
		Where("id = ?", c.ID).
		Select(campaignMutableColumns).
		Updates(c)
	if res.Error != nil {
		return fmt.Errorf("update campaign: %w", res.Error)
	}
	// Zero rows is not an error to GORM, and it is the only signal that the row is
	// gone: the service read it a moment ago, so something deleted it in between.
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: campaign %s", campaign.ErrNotFound, c.ID)
	}
	return nil
}

// Delete removes a campaign and reports how many events the foreign key's cascade
// took with it.
//
// Both statements run in one transaction, so the count is the number of rows the
// delete actually removed rather than a number that was true just before it.
func (r *CampaignRepository) Delete(ctx context.Context, id string) (int64, error) {
	var deletedEvents int64
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Counted before the delete, because afterwards the rows are gone and the
		// cascade reports nothing back: RowsAffected on the campaign delete is 1.
		if err := tx.Model(&campaign.Event{}).
			Where("campaign_id = ?", id).
			Count(&deletedEvents).Error; err != nil {
			return fmt.Errorf("count events: %w", err)
		}
		res := tx.Where("id = ?", id).Delete(&campaign.Campaign{})
		if res.Error != nil {
			return fmt.Errorf("delete campaign: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("%w: campaign %s", campaign.ErrNotFound, id)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deletedEvents, nil
}

// Exists reports whether a campaign is there, without reading the row.
func (r *CampaignRepository) Exists(ctx context.Context, id string) (bool, error) {
	var exists bool
	if err := r.db.WithContext(ctx).
		Raw("SELECT EXISTS (SELECT 1 FROM campaigns WHERE id = ?)", id).
		Scan(&exists).Error; err != nil {
		return false, fmt.Errorf("campaign exists: %w", err)
	}
	return exists, nil
}
