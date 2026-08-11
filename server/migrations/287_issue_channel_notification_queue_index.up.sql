CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_channel_notification_queue
    ON issue_channel_notification (available_at, created_at)
    WHERE status IN ('queued', 'reconciling');
