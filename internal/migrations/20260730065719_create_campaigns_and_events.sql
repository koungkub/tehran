-- The campaign aggregate: campaigns, and the events that belong to them.
--
-- lock_timeout first, as the baseline migration explains: goose wraps each file
-- in a transaction, and without it a CREATE TABLE that has to wait for a lock
-- queues ahead of every write that arrives after it. These two tables are new so
-- nothing can be holding a lock on them, but the setting costs nothing and the
-- next migration to touch a live table inherits the habit.

-- +goose Up
SET LOCAL lock_timeout = '3s';

CREATE TABLE campaigns (
    id          uuid        PRIMARY KEY,
    name        text        NOT NULL,
    description text        NOT NULL DEFAULT '',
    status      text        NOT NULL,
    -- Both nullable: a campaign with no window is open-ended, which is not the
    -- same as one starting at the epoch.
    start_at    timestamptz,
    end_at      timestamptz,
    created_at  timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL,
    -- The service validates the same two things. This is the copy that holds when
    -- a row arrives from anywhere else — a backfill, a console, the next service.
    CONSTRAINT campaigns_status_check
        CHECK (status IN ('draft', 'active', 'paused', 'ended')),
    CONSTRAINT campaigns_window_check
        CHECK (start_at IS NULL OR end_at IS NULL OR end_at > start_at)
);

-- Matches ListCampaigns' keyset order exactly, including the id tiebreak: the
-- pagination predicate is a row-value comparison on (created_at, id), and it can
-- only be answered from an index that carries both columns in that direction.
CREATE INDEX campaigns_created_at_id_idx ON campaigns (created_at DESC, id DESC);

CREATE TABLE events (
    id          uuid        PRIMARY KEY,
    -- ON DELETE CASCADE is what makes "an event cannot outlive its campaign" a
    -- property of the database rather than a rule the service remembers to apply.
    -- NOT NULL is the other half: there is no such thing as an orphan event here.
    campaign_id uuid        NOT NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    name        text        NOT NULL,
    type        text        NOT NULL DEFAULT '',
    payload     jsonb       NOT NULL DEFAULT '{}'::jsonb,
    -- When the event happened, which is not when it was written: a backfill sets
    -- it in the past, so it is the column ListEvents orders by.
    occurred_at timestamptz NOT NULL,
    created_at  timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL
);

-- Serves both ListEvents (campaign_id equality, then the keyset walk) and the
-- foreign key's own reverse lookup, which PostgreSQL does not index for you: a
-- campaign delete without this scans the whole events table.
CREATE INDEX events_campaign_occurred_idx ON events (campaign_id, occurred_at DESC, id DESC);

-- +goose Down
-- events first: it holds the foreign key, and dropping campaigns out from under
-- it would need a CASCADE that takes anything else referencing campaigns with it.
DROP TABLE events;
DROP TABLE campaigns;
