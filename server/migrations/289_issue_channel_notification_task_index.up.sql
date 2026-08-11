CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_issue_channel_notification_task
    ON issue_channel_notification (created_by_task_id)
    WHERE created_by_task_id IS NOT NULL;
