CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_issue_channel_notification_issue
    ON issue_channel_notification (issue_id);
