-- +goose Up
-- Bulletin content is permanent recorded state, not seven-day diagnostics.
CREATE TABLE bulletin_threads (
    id uuid PRIMARY KEY,
    title text NOT NULL,
    company_name text NOT NULL,
    link jsonb,
    created_at timestamptz NOT NULL,
    last_post_at timestamptz NOT NULL,
    replies integer NOT NULL CHECK (replies >= 0),
    archived boolean NOT NULL,
    removed boolean NOT NULL,
    detail_last_post_at timestamptz,
    detail_synced_at timestamptz,
    detail_replies integer,
    detail_attempted_at timestamptz,
    detail_attempts integer NOT NULL DEFAULT 0,
    invalidated_at timestamptz,
    listed_at timestamptz,
    first_seen_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);
CREATE INDEX bulletin_threads_activity_idx ON bulletin_threads (last_post_at DESC);

CREATE TABLE bulletin_posts (
    id uuid PRIMARY KEY,
    thread_id uuid NOT NULL REFERENCES bulletin_threads(id) ON DELETE CASCADE,
    company_name text NOT NULL,
    body text NOT NULL,
    is_opening boolean NOT NULL,
    created_at timestamptz NOT NULL,
    edited_at timestamptz,
    editable_until timestamptz,
    removed boolean NOT NULL,
    deleted boolean NOT NULL,
    first_seen_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);
CREATE INDEX bulletin_posts_thread_idx ON bulletin_posts (thread_id, created_at, id);

CREATE TABLE bulletin_sync (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    listed_at timestamptz,
    synced_at timestamptz,
    list_cursor text,
    head_cursor text,
    head_since timestamptz,
    head_started_at timestamptz,
    backfill_done boolean NOT NULL DEFAULT false
);
INSERT INTO bulletin_sync (singleton) VALUES (true);

-- +goose Down
DROP TABLE bulletin_sync;
DROP TABLE bulletin_posts;
DROP TABLE bulletin_threads;
