-- Durable correlation and delivery state for replying to the channel thread
-- that originated an issue. Relationships are deliberately soft: repository
-- policy forbids new foreign keys, so issue/workspace cleanup is application-
-- owned. One row is one logical success notification for one issue. A
-- concurrent unique index in the next migration is the idempotency
-- constraint; it is split out because every index in this repository is built
-- concurrently, including indexes for new tables.
CREATE TABLE issue_channel_notification (
    issue_id          UUID,
    workspace_id      UUID NOT NULL,
    created_by_task_id UUID,
    initiator_user_id UUID NOT NULL,
    chat_session_id   UUID,
    installation_id   UUID NOT NULL,
    channel_type      TEXT NOT NULL CHECK (channel_type = 'slack'),
    channel_app_id    TEXT NOT NULL,
    channel_team_id   TEXT NOT NULL,
    channel_id        TEXT NOT NULL,
    -- Slash commands have no source message timestamp, so their completion is
    -- posted at channel level. Message/app-mention dispatches always carry the
    -- immutable originating thread root here.
    thread_ts         TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL DEFAULT 'awaiting'
        CHECK (status IN ('awaiting', 'queued', 'reconciling', 'delivered', 'failed', 'cancelled')),
    available_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_token       UUID,
    lease_expires_at  TIMESTAMPTZ,
    attempts          INTEGER NOT NULL DEFAULT 0,
    reconciliation_attempts INTEGER NOT NULL DEFAULT 0,
    last_error        TEXT,
    slack_message_ts  TEXT,
    delivered_at      TIMESTAMPTZ,
    failed_at         TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    queued_at         TIMESTAMPTZ,
    cancelled_at      TIMESTAMPTZ,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Append-only operational audit. It deliberately has no foreign keys or
-- indexes: notification volumes are small, and repository policy requires
-- every index to be built concurrently in its own migration. Parent resource
-- deletion removes these rows explicitly; installation revoke/identity change
-- retains them and appends an administrative cancellation event.
CREATE TABLE issue_channel_notification_audit (
    id                 UUID NOT NULL DEFAULT gen_random_uuid(),
    issue_id           UUID,
    workspace_id       UUID NOT NULL,
    created_by_task_id UUID,
    installation_id    UUID NOT NULL,
    event_type         TEXT NOT NULL CHECK (event_type IN (
        'target_created', 'queued', 'delivery_attempt', 'delivered', 'failed', 'cancelled'
    )),
    phase              TEXT,
    attempt_number     INTEGER,
    lease_token        UUID,
    slack_message_ts   TEXT,
    error              TEXT,
    details            JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
