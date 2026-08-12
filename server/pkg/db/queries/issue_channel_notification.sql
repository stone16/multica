-- name: CreateSlackSlashIssueChannelNotificationTarget :one
-- Slack slash commands have no source message timestamp. Persist their signed,
-- installation-scoped channel as a channel-level target before returning the
-- acknowledgement, in the same transaction that created the quick-create task.
WITH inserted AS (
    INSERT INTO issue_channel_notification (
        issue_id, workspace_id, created_by_task_id, initiator_user_id,
        installation_id, channel_type, channel_app_id, channel_team_id,
        channel_id, thread_ts
    )
    SELECT
        NULL, ci.workspace_id, task.id, sqlc.arg('initiator_user_id')::uuid,
        ci.id, 'slack', ci.config ->> 'app_id', ci.config ->> 'team_id',
        sqlc.arg('channel_id')::text, ''
    FROM channel_installation ci
    JOIN agent_task_queue task ON task.id = sqlc.arg('created_by_task_id')::uuid
                              AND task.agent_id = ci.agent_id
    WHERE ci.id = sqlc.arg('installation_id')::uuid
      AND ci.workspace_id = sqlc.arg('workspace_id')::uuid
      AND ci.channel_type = 'slack'
      AND ci.status = 'active'
      AND task.originator_user_id = sqlc.arg('initiator_user_id')::uuid
      AND COALESCE(ci.config ->> 'app_id', '') <> ''
      AND COALESCE(ci.config ->> 'team_id', '') <> ''
      AND sqlc.arg('channel_id')::text <> ''
    ON CONFLICT DO NOTHING
    RETURNING *
), audited AS (
    INSERT INTO issue_channel_notification_audit (
        issue_id, workspace_id, created_by_task_id, installation_id,
        event_type, details
    )
    SELECT issue_id, workspace_id, created_by_task_id, installation_id,
           'target_created', jsonb_build_object('delivery_scope', 'channel')
    FROM inserted
)
SELECT * FROM inserted;

-- name: AttachIssueChannelNotificationFromOrigin :one
-- Resolve one logical Slack dispatch across system retry clones. A pending
-- slash target is attached to its quick-created issue exactly once; ordinary
-- Slack chat tasks and synchronous message /issue commands create their target
-- from immutable, server-trusted inbound-message provenance. The partial unique
-- task index makes a task chain capable of producing at most one notification.
WITH RECURSIVE task_lineage AS (
    SELECT task.*, 0 AS depth
    FROM agent_task_queue task
    WHERE sqlc.arg('origin_type')::text IN ('agent_create', 'quick_create')
      AND task.id = sqlc.arg('origin_id')::uuid
    UNION ALL
    SELECT parent.*, child.depth + 1
    FROM task_lineage child
    JOIN agent_task_queue parent ON parent.id = child.retry_of_task_id
), root_task AS (
    SELECT * FROM task_lineage ORDER BY depth DESC LIMIT 1
), attached_slash AS (
    UPDATE issue_channel_notification n
    SET issue_id = sqlc.arg('issue_id')::uuid,
        updated_at = now()
    FROM root_task root
    WHERE sqlc.arg('origin_type')::text = 'quick_create'
      AND n.created_by_task_id = root.id
      AND n.workspace_id = sqlc.arg('workspace_id')::uuid
      AND n.issue_id IS NULL
    RETURNING n.*
), attached_slash_audit AS (
    INSERT INTO issue_channel_notification_audit (
        issue_id, workspace_id, created_by_task_id, installation_id,
        event_type, details
    )
    SELECT issue_id, workspace_id, created_by_task_id, installation_id,
           'target_attached', jsonb_build_object('delivery_scope', 'channel')
    FROM attached_slash
    RETURNING id
), origin_message AS (
    SELECT root.id AS created_by_task_id,
           COALESCE(root.initiator_user_id, root.originator_user_id) AS initiator_user_id,
           m.chat_session_id, m.channel_message_id, m.channel_thread_id
    FROM root_task root
    JOIN chat_message m
      ON m.task_id = COALESCE(root.chat_input_task_id, root.id)
     AND m.chat_session_id = root.chat_session_id
    WHERE sqlc.arg('origin_type')::text = 'agent_create'
      AND m.role = 'user'
      AND m.channel_ingested
    ORDER BY m.created_at DESC, m.id DESC
    LIMIT 1
), direct_origin_message AS (
    SELECT NULL::uuid AS created_by_task_id,
           sqlc.arg('initiator_user_id')::uuid AS initiator_user_id,
           m.chat_session_id, m.channel_message_id, m.channel_thread_id
    FROM chat_message m
    WHERE sqlc.arg('origin_type')::text = 'slack_chat'
      AND m.id = sqlc.narg('origin_message_id')::uuid
      AND m.chat_session_id = sqlc.arg('origin_id')::uuid
      AND m.role = 'user'
      AND m.channel_ingested
), trusted_origin AS (
    SELECT * FROM origin_message
    UNION ALL
    SELECT * FROM direct_origin_message
), trusted_target AS (
    SELECT
        origin.created_by_task_id,
        origin.initiator_user_id,
        b.chat_session_id,
        b.installation_id,
        ci.config ->> 'app_id' AS channel_app_id,
        ci.config ->> 'team_id' AS channel_team_id,
        COALESCE(NULLIF(b.config ->> 'channel_id', ''),
                 split_part(b.channel_chat_id, ':', 1))::text AS channel_id,
        COALESCE(NULLIF(origin.channel_thread_id, ''),
                 NULLIF(origin.channel_message_id, ''))::text AS thread_ts
    FROM trusted_origin origin
    JOIN chat_session cs ON cs.id = origin.chat_session_id
    JOIN channel_chat_session_binding b ON b.chat_session_id = origin.chat_session_id
    JOIN channel_installation ci ON ci.id = b.installation_id
    WHERE b.channel_type = 'slack'
      AND ci.channel_type = 'slack'
      AND ci.workspace_id = sqlc.arg('workspace_id')::uuid
      AND cs.workspace_id = sqlc.arg('workspace_id')::uuid
      AND ci.status = 'active'
      AND origin.channel_message_id IS NOT NULL
      AND origin.initiator_user_id IS NOT NULL
      AND COALESCE(ci.config ->> 'app_id', '') <> ''
      AND COALESCE(ci.config ->> 'team_id', '') <> ''
), inserted AS (
    INSERT INTO issue_channel_notification (
        issue_id, workspace_id, created_by_task_id, initiator_user_id,
        chat_session_id, installation_id, channel_type,
        channel_app_id, channel_team_id, channel_id, thread_ts
    )
    SELECT
        sqlc.arg('issue_id')::uuid, sqlc.arg('workspace_id')::uuid,
        created_by_task_id, initiator_user_id, chat_session_id,
        installation_id, 'slack', channel_app_id, channel_team_id,
        channel_id, thread_ts
    FROM trusted_target
    WHERE channel_id <> '' AND thread_ts <> ''
    ON CONFLICT DO NOTHING
    RETURNING *
), attached AS (
    SELECT * FROM attached_slash
    UNION ALL
    SELECT * FROM inserted
), audited AS (
    INSERT INTO issue_channel_notification_audit (
        issue_id, workspace_id, created_by_task_id, installation_id,
        event_type, details
    )
    SELECT issue_id, workspace_id, created_by_task_id, installation_id,
           'target_created', jsonb_build_object('delivery_scope', 'thread')
    FROM inserted
)
SELECT * FROM attached;

-- name: GetIssueChannelNotificationHandoff :one
-- Return only a linked PR/MR result. Agent comments can contain customer data,
-- raw traces, or other private evidence that deterministic secret redaction
-- cannot safely classify; those details remain on the authorized issue page.
WITH target_issue AS (
    SELECT id FROM issue
    WHERE id = sqlc.arg('issue_id')::uuid
      AND workspace_id = sqlc.arg('workspace_id')::uuid
), candidates AS (
    SELECT 'pull_request'::text AS kind, pr.html_url::text AS value,
           pr.state::text AS result_state, pr.pr_updated_at AS happened_at,
           1 AS priority
    FROM github_pull_request pr
    JOIN issue_pull_request ipr ON ipr.pull_request_id = pr.id
    JOIN target_issue i ON i.id = ipr.issue_id
    WHERE NOT ipr.reference_only AND pr.html_url <> ''
    UNION ALL
    SELECT 'pull_request'::text, pr.html_url::text, pr.state::text,
           pr.pr_updated_at, 1
    FROM vcs_pull_request pr
    JOIN issue_vcs_pull_request ipr ON ipr.pull_request_id = pr.id
    JOIN target_issue i ON i.id = ipr.issue_id
    WHERE NOT ipr.reference_only AND pr.html_url <> ''
)
SELECT kind, value, result_state
FROM candidates
ORDER BY priority, happened_at DESC
LIMIT 1;

-- name: QueueIssueChannelNotification :one
-- The awaiting predicate is the idempotency transition. A repeated update,
-- event replay, later done transition, or issue reopen cannot queue it twice.
WITH updated AS (
    UPDATE issue_channel_notification AS n
    SET status = 'queued',
        available_at = now(),
        queued_at = now(),
        updated_at = now()
    WHERE n.issue_id = sqlc.arg('issue_id')
      AND n.workspace_id = sqlc.arg('workspace_id')
      AND n.status = 'awaiting'
    RETURNING *
), audited AS (
    INSERT INTO issue_channel_notification_audit (
        issue_id, workspace_id, created_by_task_id, installation_id, event_type
    )
    SELECT issue_id, workspace_id, created_by_task_id, installation_id, 'queued'
    FROM updated
)
SELECT * FROM updated;

-- name: ClaimQueuedIssueChannelNotification :one
WITH candidate AS (
    SELECT n.issue_id
    FROM issue_channel_notification n
    JOIN issue i ON i.id = n.issue_id AND i.workspace_id = n.workspace_id
    WHERE n.status IN ('queued', 'reconciling')
      AND i.status IN ('in_review', 'done')
      AND n.available_at <= now()
      AND (n.lease_expires_at IS NULL OR n.lease_expires_at <= now())
    ORDER BY n.available_at, n.created_at
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
UPDATE issue_channel_notification AS n
SET lease_token = gen_random_uuid(),
    lease_expires_at = now() + interval '2 minutes',
    claim_count = claim_count + 1,
    updated_at = now()
FROM candidate
WHERE n.issue_id = candidate.issue_id
RETURNING n.*;

-- name: AuditClaimedIssueChannelNotificationAttempt :one
-- Written immediately before the worker performs either the Slack POST path or
-- read-only reconciliation. Incrementing the matching budget and appending the
-- audit row are one statement, so a crash or audit failure before this point
-- never consumes an external delivery attempt.
WITH started AS (
    UPDATE issue_channel_notification AS n
    SET attempts = CASE WHEN n.status = 'queued' THEN n.attempts + 1 ELSE n.attempts END,
        reconciliation_attempts = CASE
            WHEN n.status = 'reconciling' THEN n.reconciliation_attempts + 1
            ELSE n.reconciliation_attempts
        END,
        updated_at = now()
    WHERE n.issue_id = sqlc.arg('issue_id')
      AND n.workspace_id = sqlc.arg('workspace_id')
      AND n.lease_token = sqlc.arg('lease_token')
      AND n.status IN ('queued', 'reconciling')
    RETURNING n.*
), audited AS (
    INSERT INTO issue_channel_notification_audit (
        issue_id, workspace_id, created_by_task_id, installation_id,
        event_type, phase, attempt_number, lease_token
    )
    SELECT issue_id, workspace_id, created_by_task_id, installation_id,
           'delivery_attempt',
           CASE WHEN status = 'reconciling' THEN 'reconcile' ELSE 'send' END,
           CASE WHEN status = 'reconciling' THEN reconciliation_attempts ELSE attempts END,
           lease_token
    FROM started
    RETURNING id
)
SELECT started.* FROM started JOIN audited ON TRUE;

-- name: LockClaimedIssueChannelNotificationForDelivery :one
-- The worker first takes a shared installation lock, then this row lock, and
-- holds both through the bounded Slack call and terminal audit write. Every
-- administrative mutation either updates the installation first or waits on
-- this row, so an accepted POST cannot be concurrently recorded as cancelled
-- or deleted.
SELECT * FROM issue_channel_notification AS n
WHERE n.issue_id = sqlc.arg('issue_id')
  AND n.workspace_id = sqlc.arg('workspace_id')
  AND n.lease_token = sqlc.arg('lease_token')
  AND n.status IN ('queued', 'reconciling')
FOR UPDATE;

-- name: DeferClaimedIssueChannelNotification :one
UPDATE issue_channel_notification
SET available_at = sqlc.arg('available_at'),
    lease_token = NULL,
    lease_expires_at = NULL,
    updated_at = now()
WHERE issue_id = sqlc.arg('issue_id')
  AND workspace_id = sqlc.arg('workspace_id')
  AND lease_token = sqlc.arg('lease_token')
  AND status IN ('queued', 'reconciling')
RETURNING *;

-- name: RetryClaimedIssueChannelNotification :one
UPDATE issue_channel_notification
SET available_at = sqlc.arg('available_at'),
    last_error = sqlc.arg('last_error'),
    lease_token = NULL,
    lease_expires_at = NULL,
    updated_at = now()
WHERE issue_id = sqlc.arg('issue_id')
  AND workspace_id = sqlc.arg('workspace_id')
  AND lease_token = sqlc.arg('lease_token')
  AND status = 'queued'
RETURNING *;

-- name: ReconcileClaimedIssueChannelNotification :one
UPDATE issue_channel_notification
SET status = 'reconciling',
    available_at = sqlc.arg('available_at'),
    last_error = sqlc.arg('last_error'),
    lease_token = NULL,
    lease_expires_at = NULL,
    updated_at = now()
WHERE issue_id = sqlc.arg('issue_id')
  AND workspace_id = sqlc.arg('workspace_id')
  AND lease_token = sqlc.arg('lease_token')
  AND status = 'queued'
RETURNING *;

-- name: RetryReconcilingIssueChannelNotification :one
UPDATE issue_channel_notification
SET available_at = sqlc.arg('available_at'),
    last_error = sqlc.arg('last_error'),
    lease_token = NULL,
    lease_expires_at = NULL,
    updated_at = now()
WHERE issue_id = sqlc.arg('issue_id')
  AND workspace_id = sqlc.arg('workspace_id')
  AND lease_token = sqlc.arg('lease_token')
  AND status = 'reconciling'
RETURNING *;

-- name: DeliverClaimedIssueChannelNotification :one
WITH delivered AS (
    UPDATE issue_channel_notification AS n
    SET status = 'delivered',
        last_error = NULL,
        slack_message_ts = sqlc.arg('slack_message_ts'),
        delivered_at = now(),
        lease_token = NULL,
        lease_expires_at = NULL,
        updated_at = now()
    WHERE n.issue_id = sqlc.arg('issue_id')
      AND n.workspace_id = sqlc.arg('workspace_id')
      AND n.lease_token = sqlc.arg('lease_token')
      AND n.status = 'queued'
    RETURNING n.*
), audited AS (
    INSERT INTO issue_channel_notification_audit (
        issue_id, workspace_id, created_by_task_id, installation_id,
        event_type, phase, attempt_number, slack_message_ts
    )
    SELECT issue_id, workspace_id, created_by_task_id, installation_id,
           'delivered', 'send', attempts, slack_message_ts
    FROM delivered
)
SELECT issue_id FROM delivered;

-- name: DeliverReconcilingIssueChannelNotification :one
WITH delivered AS (
    UPDATE issue_channel_notification AS n
    SET status = 'delivered',
        last_error = NULL,
        slack_message_ts = sqlc.arg('slack_message_ts'),
        delivered_at = now(),
        lease_token = NULL,
        lease_expires_at = NULL,
        updated_at = now()
    WHERE n.issue_id = sqlc.arg('issue_id')
      AND n.workspace_id = sqlc.arg('workspace_id')
      AND n.lease_token = sqlc.arg('lease_token')
      AND n.status = 'reconciling'
    RETURNING n.*
), audited AS (
    INSERT INTO issue_channel_notification_audit (
        issue_id, workspace_id, created_by_task_id, installation_id,
        event_type, phase, attempt_number, slack_message_ts
    )
    SELECT issue_id, workspace_id, created_by_task_id, installation_id,
           'delivered', 'reconcile', reconciliation_attempts, sqlc.arg('slack_message_ts')
    FROM delivered
)
SELECT issue_id FROM delivered;

-- name: FailClaimedIssueChannelNotification :one
WITH failed AS (
    UPDATE issue_channel_notification AS n
    SET status = 'failed',
        last_error = sqlc.arg('last_error'),
        failed_at = now(),
        lease_token = NULL,
        lease_expires_at = NULL,
        updated_at = now()
    WHERE n.issue_id = sqlc.arg('issue_id')
      AND n.workspace_id = sqlc.arg('workspace_id')
      AND n.lease_token = sqlc.arg('lease_token')
      AND n.status = 'queued'
    RETURNING n.*
), audited AS (
    INSERT INTO issue_channel_notification_audit (
        issue_id, workspace_id, created_by_task_id, installation_id,
        event_type, phase, attempt_number, error
    )
    SELECT issue_id, workspace_id, created_by_task_id, installation_id,
           'failed', 'send', attempts, last_error
    FROM failed
)
SELECT issue_id FROM failed;

-- name: FailReconcilingIssueChannelNotification :one
WITH failed AS (
    UPDATE issue_channel_notification AS n
    SET status = 'failed',
        last_error = sqlc.arg('last_error'),
        failed_at = now(),
        lease_token = NULL,
        lease_expires_at = NULL,
        updated_at = now()
    WHERE n.issue_id = sqlc.arg('issue_id')
      AND n.workspace_id = sqlc.arg('workspace_id')
      AND n.lease_token = sqlc.arg('lease_token')
      AND n.status = 'reconciling'
    RETURNING n.*
), audited AS (
    INSERT INTO issue_channel_notification_audit (
        issue_id, workspace_id, created_by_task_id, installation_id,
        event_type, phase, attempt_number, error
    )
    SELECT issue_id, workspace_id, created_by_task_id, installation_id,
           'failed', 'reconcile', reconciliation_attempts, last_error
    FROM failed
)
SELECT issue_id FROM failed;

-- name: CancelIssueChannelNotificationsByInstallation :exec
WITH cancelled AS (
    UPDATE issue_channel_notification AS n
    SET status = 'cancelled',
        cancelled_at = now(),
        last_error = 'Slack installation administratively cancelled',
        lease_token = NULL,
        lease_expires_at = NULL,
        updated_at = now()
    WHERE n.installation_id = sqlc.arg('installation_id')
      AND n.status IN ('awaiting', 'queued', 'reconciling')
    RETURNING *
)
INSERT INTO issue_channel_notification_audit (
    issue_id, workspace_id, created_by_task_id, installation_id,
    event_type, error, details
)
SELECT issue_id, workspace_id, created_by_task_id, installation_id,
       'cancelled', last_error, jsonb_build_object('reason', sqlc.arg('reason')::text)
FROM cancelled;

-- name: CancelIssueChannelNotificationsByChatSession :exec
WITH cancelled AS (
    UPDATE issue_channel_notification
    SET status = 'cancelled',
        cancelled_at = now(),
        last_error = 'Originating chat session administratively cancelled',
        lease_token = NULL,
        lease_expires_at = NULL,
        updated_at = now()
    WHERE chat_session_id = sqlc.arg('chat_session_id')
      AND status IN ('awaiting', 'queued', 'reconciling')
    RETURNING *
)
INSERT INTO issue_channel_notification_audit (
    issue_id, workspace_id, created_by_task_id, installation_id,
    event_type, error, details
)
SELECT issue_id, workspace_id, created_by_task_id, installation_id,
       'cancelled', last_error, jsonb_build_object('reason', sqlc.arg('reason')::text)
FROM cancelled;

-- name: DeleteIssueChannelNotificationByIssue :exec
WITH deleted AS (
    DELETE FROM issue_channel_notification AS n
    WHERE n.issue_id = sqlc.arg('issue_id') AND n.workspace_id = sqlc.arg('workspace_id')
    RETURNING n.created_by_task_id
)
DELETE FROM issue_channel_notification_audit
WHERE issue_channel_notification_audit.workspace_id = sqlc.arg('workspace_id')
  AND (
      issue_channel_notification_audit.issue_id = sqlc.arg('issue_id')
      OR issue_channel_notification_audit.created_by_task_id IN (
          SELECT created_by_task_id FROM deleted WHERE created_by_task_id IS NOT NULL
      )
  );

-- name: DeleteIssueChannelNotificationsByWorkspace :exec
WITH deleted AS (
    DELETE FROM issue_channel_notification AS n WHERE n.workspace_id = sqlc.arg('workspace_id')
)
DELETE FROM issue_channel_notification_audit
WHERE issue_channel_notification_audit.workspace_id = sqlc.arg('workspace_id');
