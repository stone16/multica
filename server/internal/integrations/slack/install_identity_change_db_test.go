package slack

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func slackInstallTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("no database: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("database not reachable: %v", err)
	}
	var migrated bool
	if err := pool.QueryRow(ctx, `
SELECT to_regclass('public.channel_installation') IS NOT NULL
   AND to_regclass('public.channel_media_pending_object') IS NOT NULL
   AND to_regclass('public.issue_channel_notification') IS NOT NULL
`).Scan(&migrated); err != nil || !migrated {
		pool.Close()
		t.Skip("Slack installation tables not present (database not migrated)")
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestRegisterBYO_ChangedSlackIdentityClearsInstallationOwnedState(t *testing.T) {
	pool := slackInstallTestDB(t)
	ctx := context.Background()
	const (
		workspaceID = "5c09e300-0000-4000-8000-000000000001"
		agentID     = "5c09e300-0000-4000-8000-000000000002"
		installerID = "5c09e300-0000-4000-8000-000000000003"
		userID      = "5c09e300-0000-4000-8000-000000000004"
		sessionID   = "5c09e300-0000-4000-8000-000000000005"
		issueID     = "5c09e300-0000-4000-8000-000000000006"
		oldAppID    = "A0MULTICACLEANOLD"
		newAppID    = "A0MULTICACLEANNEW"
		tokenHash   = "slack_identity_change_token"
		dedupID     = "slack_identity_change_message"
		auditID     = "slack_identity_change_event"
		storageKey  = "slack/identity-change/pending-object"
	)

	clean := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM issue_channel_notification_audit WHERE issue_id = $1`, issueID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM issue_channel_notification WHERE issue_id = $1`, issueID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM channel_outbound_card_message WHERE chat_session_id = $1`, sessionID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM channel_chat_session_binding WHERE chat_session_id = $1`, sessionID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM channel_user_binding WHERE multica_user_id = $1`, userID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM channel_binding_token WHERE token_hash = $1`, tokenHash)
		_, _ = pool.Exec(context.Background(), `DELETE FROM channel_inbound_message_dedup WHERE message_id = $1`, dedupID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM channel_inbound_audit WHERE channel_event_id = $1`, auditID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM channel_media_pending_object WHERE storage_key = $1`, storageKey)
		_, _ = pool.Exec(context.Background(), `DELETE FROM channel_installation WHERE workspace_id = $1 AND agent_id = $2 AND channel_type = 'slack'`, workspaceID, agentID)
	}
	clean()
	t.Cleanup(clean)

	var installationID string
	if err := pool.QueryRow(ctx, `
INSERT INTO channel_installation (workspace_id, agent_id, channel_type, config, installer_user_id, status)
VALUES ($1, $2, 'slack', jsonb_build_object('app_id', $3::text, 'team_id', 'T-OLD'), $4, 'active')
RETURNING id
`, workspaceID, agentID, oldAppID, installerID).Scan(&installationID); err != nil {
		t.Fatalf("seed Slack installation: %v", err)
	}
	seed := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatalf("seed installation-owned state: %v", err)
		}
	}
	seed(`INSERT INTO channel_user_binding
    (workspace_id, multica_user_id, installation_id, channel_type, channel_user_id)
VALUES ($1, $2, $3, 'slack', 'U-SAME-IN-NEW-TEAM')`, workspaceID, userID, installationID)
	seed(`INSERT INTO channel_binding_token
    (token_hash, workspace_id, installation_id, channel_type, channel_user_id, expires_at)
VALUES ($1, $2, $3, 'slack', 'U-SAME-IN-NEW-TEAM', now() + interval '10 minutes')`, tokenHash, workspaceID, installationID)
	seed(`INSERT INTO channel_inbound_message_dedup (installation_id, message_id)
VALUES ($1, $2)`, installationID, dedupID)
	seed(`INSERT INTO channel_inbound_audit
    (installation_id, channel_type, event_type, channel_event_id, drop_reason)
VALUES ($1, 'slack', 'message', $2, 'test')`, installationID, auditID)
	seed(`INSERT INTO channel_media_pending_object
    (storage_key, workspace_id, chat_message_id, storage_url, installation_id)
VALUES ($1, $2, $3, 's3://test/slack-identity-change', $4)`, storageKey, workspaceID, sessionID, installationID)
	seed(`INSERT INTO channel_chat_session_binding
    (chat_session_id, installation_id, channel_type, channel_chat_id, chat_type)
VALUES ($1, $2, 'slack', 'D-OLD', 'p2p')`, sessionID, installationID)
	seed(`INSERT INTO channel_outbound_card_message
    (chat_session_id, channel_type, channel_chat_id, channel_card_message_id, status)
VALUES ($1, 'slack', 'D-OLD', 'old-card', 'final')`, sessionID)
	seed(`INSERT INTO issue_channel_notification
    (issue_id, workspace_id, initiator_user_id, chat_session_id, installation_id,
     channel_type, channel_app_id, channel_team_id, channel_id, thread_ts)
VALUES ($1, $2, $3, $4, $5, 'slack', $6, 'T-OLD', 'D-OLD', '1700000000.000001')`,
		issueID, workspaceID, userID, sessionID, installationID, oldAppID)

	srv := slackMockServer(t, slackMock{authOK: true, botAppID: newAppID, appTokenOK: true})
	defer srv.Close()
	svc, err := NewInstallService(db.New(pool), pool, testBox(t), nil)
	if err != nil {
		t.Fatalf("NewInstallService: %v", err)
	}
	svc.apiURL = srv.URL + "/"
	row, err := svc.RegisterBYO(ctx, RegisterBYOParams{
		WorkspaceID: pgtypeUUID(workspaceID),
		AgentID:     pgtypeUUID(agentID),
		InitiatorID: pgtypeUUID(installerID),
		BotToken:    "xoxb-replacement-bot-token",
		AppToken:    "xapp-1-" + newAppID + "-111-replacement-secret",
	})
	if err != nil {
		t.Fatalf("RegisterBYO changed identity: %v", err)
	}
	if got := util.UUIDToString(row.ID); got != installationID {
		t.Fatalf("installation id = %s, want in-place row %s", got, installationID)
	}

	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
			t.Fatalf("count installation-owned state: %v", err)
		}
		return n
	}
	for name, query := range map[string]string{
		"user bindings":  `SELECT count(*) FROM channel_user_binding WHERE installation_id = $1`,
		"binding tokens": `SELECT count(*) FROM channel_binding_token WHERE installation_id = $1`,
		"inbound dedup":  `SELECT count(*) FROM channel_inbound_message_dedup WHERE installation_id = $1`,
		"chat routing":   `SELECT count(*) FROM channel_chat_session_binding WHERE installation_id = $1`,
	} {
		if n := count(query, installationID); n != 0 {
			t.Errorf("old %s survived changed Slack identity: %d rows", name, n)
		}
	}
	if n := count(`SELECT count(*) FROM issue_channel_notification WHERE installation_id = $1 AND status = 'cancelled'`, installationID); n != 1 {
		t.Errorf("old issue notification should survive as cancelled audit state, got %d rows", n)
	}
	if n := count(`SELECT count(*) FROM issue_channel_notification_audit
WHERE issue_id = $1 AND event_type = 'cancelled'
  AND details ->> 'reason' = 'installation_identity_changed'`, issueID); n != 1 {
		t.Errorf("old issue notification cancellation audit rows = %d, want 1", n)
	}
	if n := count(`SELECT count(*) FROM channel_outbound_card_message WHERE chat_session_id = $1`, sessionID); n != 0 {
		t.Errorf("old outbound cards survived changed Slack identity: %d rows", n)
	}
	if n := count(`SELECT count(*) FROM channel_inbound_audit WHERE channel_event_id = $1 AND installation_id IS NULL`, auditID); n != 1 {
		t.Errorf("old audit should survive detached from the reused installation, got %d rows", n)
	}
	if n := count(`SELECT count(*) FROM channel_media_pending_object WHERE storage_key = $1 AND installation_id IS NULL`, storageKey); n != 1 {
		t.Errorf("old media intent should survive detached from the reused installation, got %d rows", n)
	}
}
