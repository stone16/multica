package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	channelslack "github.com/multica-ai/multica/server/internal/integrations/slack"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestIssueChannelNotification_QueuesSlackOriginOnce exercises the public issue
// create/update boundary. A Slack chat task is server-trusted provenance; the
// issue it creates must remember that exact thread, and the first transition to
// in_review must durably queue one notification. Later status changes must not
// create another logical success notification.
func TestIssueChannelNotification_QueuesSlackOriginOnce(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID := createHandlerTestAgent(t, "Slack completion notification agent", nil)

	var installationID string
	config := map[string]any{
		"app_id":              "A-completion-test",
		"team_id":             "T-completion-test",
		"bot_token_encrypted": base64.StdEncoding.EncodeToString([]byte("xoxb-completion-test")),
	}
	configJSON, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshal Slack installation config: %v", err)
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO channel_installation (
			workspace_id, agent_id, channel_type, config, installer_user_id
		) VALUES ($1, $2, 'slack', $3, $4)
		RETURNING id
	`, testWorkspaceID, agentID, configJSON, testUserID).Scan(&installationID); err != nil {
		t.Fatalf("create Slack installation: %v", err)
	}

	if _, err := testPool.Exec(ctx, `
		INSERT INTO channel_user_binding (
			workspace_id, multica_user_id, installation_id, channel_type, channel_user_id
		) VALUES ($1, $2, $3, 'slack', 'U-completion-requester')
	`, testWorkspaceID, testUserID, installationID); err != nil {
		t.Fatalf("bind Slack requester: %v", err)
	}

	// Start at the real Slack Events API boundary. The production translator,
	// installation/team resolver, identity resolver, dedup gate, session binder,
	// immutable message append, and TaskService enqueue all participate before
	// the agent-facing issue API is invoked below.
	wireEvent := json.RawMessage(`{
		"token":"verification-is-disabled-in-this-test",
		"team_id":"T-completion-test",
		"api_app_id":"A-completion-test",
		"type":"event_callback",
		"event_id":"Ev-completion-test",
		"event_time":1700000000,
		"event":{
			"type":"app_mention",
			"user":"U-completion-requester",
			"text":"<@U-completion-bot> create the requested issue",
			"ts":"1700000000.000200",
			"thread_ts":"1700000000.000100",
			"channel":"C-completion-test",
			"event_ts":"1700000000.000200"
		}
	}`)
	event, err := slackevents.ParseEvent(wireEvent, slackevents.OptionNoVerifyToken())
	if err != nil {
		t.Fatalf("parse Slack Events API payload: %v", err)
	}
	inbound, ok := channelslack.TranslateEventsAPI(event, "U-completion-bot")
	if !ok {
		t.Fatal("Slack Events API payload was not ingestable")
	}
	channelRouter := engine.NewRouter(testHandler.IssueService, testHandler.TaskService, testHandler.Queries, engine.RouterConfig{})
	channelRouter.Register(channelslack.TypeSlack, channelslack.NewSlackResolverSet(testHandler.Queries, testPool, nil, nil))
	if err := channelRouter.Handle(ctx, inbound); err != nil {
		t.Fatalf("route Slack Events API payload: %v", err)
	}

	var sessionID, rootTaskID string
	var storedMessageID, storedThreadID string
	var channelIngested bool
	if err := testPool.QueryRow(ctx, `
		SELECT m.chat_session_id, m.task_id, m.channel_ingested,
		       m.channel_message_id, m.channel_thread_id
		FROM chat_message m
		JOIN channel_chat_session_binding b ON b.chat_session_id = m.chat_session_id
		WHERE b.installation_id = $1
		  AND m.role = 'user'
		  AND m.channel_message_id = '1700000000.000200'
	`, installationID).Scan(&sessionID, &rootTaskID, &channelIngested, &storedMessageID, &storedThreadID); err != nil {
		t.Fatalf("load Slack-ingested task input: %v", err)
	}
	if !channelIngested || storedMessageID != "1700000000.000200" || storedThreadID != "1700000000.000100" {
		t.Fatalf("Slack task provenance = ingested %t message %q thread %q", channelIngested, storedMessageID, storedThreadID)
	}
	// The first attempt fails before creating the issue. The system retry clone
	// must still resolve the immutable Slack input owned by the root dispatch.
	if _, err := testPool.Exec(ctx, `
		UPDATE agent_task_queue
		SET status = 'failed', failure_reason = 'agent_error.provider_network', completed_at = now()
		WHERE id = $1
	`, rootTaskID); err != nil {
		t.Fatalf("fail root Slack task: %v", err)
	}
	retryTask, err := testHandler.Queries.CreateRetryTask(ctx, db.CreateRetryTaskParams{ID: parseUUID(rootTaskID)})
	if err != nil {
		t.Fatalf("create Slack task retry clone: %v", err)
	}
	taskID := uuidToString(retryTask.ID)
	// A later DM turn advances the mutable outbound reply target before the
	// agent finishes handling message A. Correlation must stay on A.
	if _, err := testPool.Exec(ctx, `
		UPDATE channel_chat_session_binding
		SET last_message_id = $2, last_thread_id = NULL
		WHERE chat_session_id = $1
	`, sessionID, "1700000000.000300"); err != nil {
		t.Fatalf("advance mutable Slack reply target: %v", err)
	}

	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":  "Notify the originating Slack thread",
		"status": "in_progress",
	})
	req.Header.Set("X-Agent-ID", agentID)
	req.Header.Set("X-Task-ID", taskID)
	testHandler.CreateIssue(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateIssue: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var created IssueResponse
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("decode issue: %v", err)
	}

	t.Cleanup(func() {
		// New notification rows have no database cascade by design.
		cleanup := []struct {
			name string
			sql  string
			arg  string
		}{
			{"notification audit", `DELETE FROM issue_channel_notification_audit WHERE issue_id = $1`, created.ID},
			{"notification", `DELETE FROM issue_channel_notification WHERE issue_id = $1`, created.ID},
			{"issue", `DELETE FROM issue WHERE id = $1`, created.ID},
			{"message", `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID},
			{"task", `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, sessionID},
			{"dedup", `DELETE FROM channel_inbound_message_dedup WHERE installation_id = $1`, installationID},
			{"user binding", `DELETE FROM channel_user_binding WHERE installation_id = $1`, installationID},
			{"chat binding", `DELETE FROM channel_chat_session_binding WHERE chat_session_id = $1`, sessionID},
			{"chat session", `DELETE FROM chat_session WHERE id = $1`, sessionID},
			{"installation", `DELETE FROM channel_installation WHERE id = $1`, installationID},
		}
		for _, item := range cleanup {
			if _, err := testPool.Exec(context.Background(), item.sql, item.arg); err != nil {
				t.Errorf("cleanup %s: %v", item.name, err)
			}
		}
	})

	assertNotification := func(wantStatus string, wantCount int) {
		t.Helper()
		var count int
		var status, channelID, threadTS string
		err := testPool.QueryRow(ctx, `
			SELECT count(*)::int,
			       COALESCE(min(status), ''),
			       COALESCE(min(channel_id), ''),
			       COALESCE(min(thread_ts), '')
			FROM issue_channel_notification
			WHERE issue_id = $1
		`, created.ID).Scan(&count, &status, &channelID, &threadTS)
		if err != nil {
			t.Fatalf("load issue channel notification: %v", err)
		}
		if count != wantCount || status != wantStatus {
			t.Fatalf("notification = count %d status %q, want count %d status %q", count, status, wantCount, wantStatus)
		}
		if channelID != "C-completion-test" || threadTS != "1700000000.000100" {
			t.Fatalf("notification target = channel %q thread %q, want original Slack thread", channelID, threadTS)
		}
	}
	assertAttempts := func(want int) {
		t.Helper()
		var attempts int
		if err := testPool.QueryRow(ctx, `SELECT attempts FROM issue_channel_notification WHERE issue_id = $1`, created.ID).Scan(&attempts); err != nil {
			t.Fatalf("load notification attempts: %v", err)
		}
		if attempts != want {
			t.Fatalf("notification attempts = %d, want %d", attempts, want)
		}
	}

	assertNotification("awaiting", 1)
	var notificationTaskID string
	if err := testPool.QueryRow(ctx, `
		SELECT created_by_task_id FROM issue_channel_notification WHERE issue_id = $1
	`, created.ID).Scan(&notificationTaskID); err != nil {
		t.Fatalf("load notification dispatch root: %v", err)
	}
	if notificationTaskID != rootTaskID {
		t.Fatalf("notification dispatch task = %s, want retry root %s", notificationTaskID, rootTaskID)
	}

	// A single Slack dispatch may create at most one callback target even if a
	// retrying agent mistakenly creates another issue.
	duplicateDispatchCreate := httptest.NewRecorder()
	duplicateDispatchReq := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title": "Second issue from the same Slack dispatch",
	})
	duplicateDispatchReq.Header.Set("X-Agent-ID", agentID)
	duplicateDispatchReq.Header.Set("X-Task-ID", taskID)
	testHandler.CreateIssue(duplicateDispatchCreate, duplicateDispatchReq)
	if duplicateDispatchCreate.Code != http.StatusCreated {
		t.Fatalf("CreateIssue(second dispatch issue): expected 201, got %d: %s", duplicateDispatchCreate.Code, duplicateDispatchCreate.Body.String())
	}
	var duplicateDispatchIssue IssueResponse
	if err := json.NewDecoder(duplicateDispatchCreate.Body).Decode(&duplicateDispatchIssue); err != nil {
		t.Fatalf("decode second dispatch issue: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, duplicateDispatchIssue.ID)
	})
	var dispatchNotificationCount, secondIssueNotificationCount int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM issue_channel_notification WHERE created_by_task_id = $1
	`, rootTaskID).Scan(&dispatchNotificationCount); err != nil {
		t.Fatalf("count root dispatch notifications: %v", err)
	}
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM issue_channel_notification WHERE issue_id = $1
	`, duplicateDispatchIssue.ID).Scan(&secondIssueNotificationCount); err != nil {
		t.Fatalf("count second issue notifications: %v", err)
	}
	if dispatchNotificationCount != 1 || secondIssueNotificationCount != 0 {
		t.Fatalf("dispatch notifications = %d and second issue notifications = %d, want 1/0", dispatchNotificationCount, secondIssueNotificationCount)
	}
	if _, err := testPool.Exec(ctx, `
		UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1
	`, taskID); err != nil {
		t.Fatalf("start originating chat task: %v", err)
	}
	if _, err := testHandler.TaskService.CompleteTask(ctx, parseUUID(taskID), nil, "", "", false, ""); err != nil {
		t.Fatalf("complete originating chat task: %v", err)
	}
	assertNotification("awaiting", 1)
	var issueStatus, taskStatus string
	if err := testPool.QueryRow(ctx, `
		SELECT i.status, t.status FROM issue i, agent_task_queue t
		WHERE i.id = $1 AND t.id = $2
	`, created.ID, taskID).Scan(&issueStatus, &taskStatus); err != nil {
		t.Fatalf("load issue/task status after task completion: %v", err)
	}
	if issueStatus != "in_progress" || taskStatus != "completed" {
		t.Fatalf("task completion state = issue %q task %q, want in_progress/completed", issueStatus, taskStatus)
	}

	updateStatus := func(status string) {
		t.Helper()
		w := httptest.NewRecorder()
		req := newRequest("PUT", "/api/issues/"+created.ID, map[string]any{"status": status})
		req = withURLParam(req, "id", created.ID)
		testHandler.UpdateIssue(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("UpdateIssue(%s): expected 200, got %d: %s", status, w.Code, w.Body.String())
		}
	}

	updateStatus("in_review")
	assertNotification("queued", 1)
	var slackMu sync.Mutex
	var slackPost url.Values
	slackAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.replies":
			_, _ = w.Write([]byte(`{"ok":true,"messages":[],"has_more":false}`))
		case "/chat.postMessage":
			slackMu.Lock()
			slackPost = r.PostForm
			slackMu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true,"channel":"C-completion-test","ts":"1700000000.999999"}`))
		default:
			http.Error(w, "unexpected Slack method", http.StatusNotFound)
		}
	}))
	defer slackAPI.Close()
	sender := channelslack.NewIssueNotification(
		testHandler.Queries,
		nil,
		"https://app.multica.test",
		channelslack.WithIssueNotificationAPIURL(slackAPI.URL),
	)
	worker := NewIssueChannelNotificationWorker(testHandler.Queries, sender)
	updateStatus("todo")
	worked, err := worker.ProcessNext(ctx)
	if err != nil || worked {
		t.Fatalf("ProcessNext while reopened = (%v, %v), want queued row left unclaimed", worked, err)
	}
	assertNotification("queued", 1)
	updateStatus("in_review")
	worked, err = worker.ProcessNext(ctx)
	if err != nil || !worked {
		t.Fatalf("ProcessNext = (%v, %v), want delivered work", worked, err)
	}
	slackMu.Lock()
	posted := slackPost
	slackMu.Unlock()
	if posted.Get("channel") != "C-completion-test" || posted.Get("thread_ts") != "1700000000.000100" {
		t.Fatalf("worker posted to channel %q thread %q, want originating Slack thread", posted.Get("channel"), posted.Get("thread_ts"))
	}
	if got := posted.Get("metadata"); got == "" {
		t.Fatal("Slack post omitted idempotency metadata")
	}
	assertNotification("delivered", 1)
	assertAttempts(1)
	var auditEvents []string
	rows, err := testPool.Query(ctx, `
SELECT event_type FROM issue_channel_notification_audit
WHERE issue_id = $1
ORDER BY created_at, id
`, created.ID)
	if err != nil {
		t.Fatalf("load notification audit trail: %v", err)
	}
	for rows.Next() {
		var event string
		if err := rows.Scan(&event); err != nil {
			rows.Close()
			t.Fatalf("scan notification audit trail: %v", err)
		}
		auditEvents = append(auditEvents, event)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("iterate notification audit trail: %v", err)
	}
	rows.Close()
	wantAuditEvents := []string{"target_created", "queued", "delivery_attempt", "delivered"}
	if len(auditEvents) != len(wantAuditEvents) {
		t.Fatalf("notification audit events = %v, want %v", auditEvents, wantAuditEvents)
	}
	for i := range wantAuditEvents {
		if auditEvents[i] != wantAuditEvents[i] {
			t.Fatalf("notification audit events = %v, want %v", auditEvents, wantAuditEvents)
		}
	}
	worked, err = worker.ProcessNext(ctx)
	if err != nil || worked {
		t.Fatalf("replay ProcessNext = (%v, %v), want no duplicate work", worked, err)
	}
	assertAttempts(1)
	updateStatus("todo")
	updateStatus("in_review")
	assertNotification("delivered", 1)
	updateStatus("done")
	assertNotification("delivered", 1)

	// A web/mobile task can reuse this same channel-bound session. Without an
	// immutable channel_ingested input it must not gain a Slack notification.
	var directTaskID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (
			agent_id, runtime_id, chat_session_id, status, priority, started_at,
			originator_user_id, accountable_user_id
		) VALUES ($1, $2, $3, 'running', 0, now(), $4, $4)
		RETURNING id
	`, agentID, handlerTestRuntimeID(t), sessionID, testUserID).Scan(&directTaskID); err != nil {
		t.Fatalf("create direct chat task: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE agent_task_queue SET chat_input_task_id = id WHERE id = $1`, directTaskID); err != nil {
		t.Fatalf("set direct task input owner: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO chat_message (chat_session_id, role, content, task_id)
		VALUES ($1, 'user', 'web input', $2)
	`, sessionID, directTaskID); err != nil {
		t.Fatalf("create direct task input: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM chat_message WHERE task_id = $1`, directTaskID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, directTaskID)
	})

	directCreate := httptest.NewRecorder()
	directReq := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title": "Do not notify Slack for direct chat input",
	})
	directReq.Header.Set("X-Agent-ID", agentID)
	directReq.Header.Set("X-Task-ID", directTaskID)
	testHandler.CreateIssue(directCreate, directReq)
	if directCreate.Code != http.StatusCreated {
		t.Fatalf("CreateIssue(direct): expected 201, got %d: %s", directCreate.Code, directCreate.Body.String())
	}
	var directCreated IssueResponse
	if err := json.NewDecoder(directCreate.Body).Decode(&directCreated); err != nil {
		t.Fatalf("decode direct issue: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, directCreated.ID)
	})
	var directNotificationCount int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM issue_channel_notification WHERE issue_id = $1`, directCreated.ID).Scan(&directNotificationCount); err != nil {
		t.Fatalf("count direct issue notifications: %v", err)
	}
	if directNotificationCount != 0 {
		t.Fatalf("direct chat issue notifications = %d, want 0", directNotificationCount)
	}
}

// TestIssueChannelNotification_SlackSlashCommandPostsChannelCallback covers the
// native Slack /issue boundary. Slack provides no source message timestamp for
// slash commands, so the callback must be durable before acknowledgement and
// later post to the signed command channel without fabricating a thread.
func TestIssueChannelNotification_SlackSlashCommandPostsChannelCallback(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID := createHandlerTestAgent(t, "Slack slash completion notification agent", nil)

	var installationID string
	config := map[string]any{
		"app_id":              "A-slash-completion-test",
		"team_id":             "T-slash-completion-test",
		"bot_token_encrypted": base64.StdEncoding.EncodeToString([]byte("xoxb-slash-completion-test")),
	}
	configJSON, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshal Slack slash installation config: %v", err)
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO channel_installation (
			workspace_id, agent_id, channel_type, config, installer_user_id
		) VALUES ($1, $2, 'slack', $3, $4)
		RETURNING id
	`, testWorkspaceID, agentID, configJSON, testUserID).Scan(&installationID); err != nil {
		t.Fatalf("create Slack slash installation: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO channel_user_binding (
			workspace_id, multica_user_id, installation_id, channel_type, channel_user_id
		) VALUES ($1, $2, $3, 'slack', 'U-slash-completion-requester')
	`, testWorkspaceID, testUserID, installationID); err != nil {
		t.Fatalf("bind Slack slash requester: %v", err)
	}

	var ackMu sync.Mutex
	ackCount := 0
	ack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ackMu.Lock()
		ackCount++
		ackMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer ack.Close()
	processor := channelslack.NewSlashCommandProcessor(channelslack.SlashCommandConfig{
		Queries: testHandler.Queries,
		Tasks:   testHandler.TaskService,
	})
	processor.Handle(ctx, slack.SlashCommand{
		Command:     "/issue",
		Text:        "Notify the originating channel when this is reviewed",
		APIAppID:    "A-slash-completion-test",
		TeamID:      "T-slash-completion-test",
		UserID:      "U-slash-completion-requester",
		ChannelID:   "C-slash-completion-test",
		ResponseURL: ack.URL,
	})
	ackMu.Lock()
	gotAckCount := ackCount
	ackMu.Unlock()
	if gotAckCount != 1 {
		t.Fatalf("slash command acknowledgements = %d, want 1", gotAckCount)
	}

	var taskID string
	var pendingIssueID *string
	var targetChannel, targetThread string
	if err := testPool.QueryRow(ctx, `
		SELECT created_by_task_id, issue_id::text, channel_id, thread_ts
		FROM issue_channel_notification
		WHERE installation_id = $1
	`, installationID).Scan(&taskID, &pendingIssueID, &targetChannel, &targetThread); err != nil {
		t.Fatalf("load slash callback target: %v", err)
	}
	if pendingIssueID != nil || targetChannel != "C-slash-completion-test" || targetThread != "" {
		t.Fatalf("slash target = issue %v channel %q thread %q, want pending/channel-level", pendingIssueID, targetChannel, targetThread)
	}

	var created IssueResponse
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM issue_channel_notification_audit WHERE created_by_task_id = $1`, taskID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM issue_channel_notification WHERE created_by_task_id = $1`, taskID)
		if created.ID != "" {
			_, _ = testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, created.ID)
		}
		_, _ = testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM channel_user_binding WHERE installation_id = $1`, installationID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM channel_installation WHERE id = $1`, installationID)
	})

	create := httptest.NewRecorder()
	createReq := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":       "Native Slack slash callback",
		"status":      "in_review",
		"origin_type": "quick_create",
		"origin_id":   taskID,
	})
	createReq.Header.Set("X-Agent-ID", agentID)
	createReq.Header.Set("X-Task-ID", taskID)
	testHandler.CreateIssue(create, createReq)
	if create.Code != http.StatusCreated {
		t.Fatalf("CreateIssue(slash): expected 201, got %d: %s", create.Code, create.Body.String())
	}
	if err := json.NewDecoder(create.Body).Decode(&created); err != nil {
		t.Fatalf("decode slash issue: %v", err)
	}

	var slackMu sync.Mutex
	var slackPost url.Values
	slackAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.history":
			_, _ = w.Write([]byte(`{"ok":true,"messages":[],"has_more":false}`))
			return
		case "/chat.postMessage":
		default:
			http.Error(w, "unexpected Slack method", http.StatusNotFound)
			return
		}
		slackMu.Lock()
		slackPost = r.PostForm
		slackMu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C-slash-completion-test","ts":"1700000001.999999"}`))
	}))
	defer slackAPI.Close()
	sender := channelslack.NewIssueNotification(
		testHandler.Queries,
		nil,
		"https://app.multica.test",
		channelslack.WithIssueNotificationAPIURL(slackAPI.URL),
	)
	worker := NewIssueChannelNotificationWorker(testHandler.Queries, sender)
	worked, err := worker.ProcessNext(ctx)
	if err != nil || !worked {
		t.Fatalf("ProcessNext(slash) = (%v, %v), want delivered work", worked, err)
	}
	slackMu.Lock()
	posted := slackPost
	slackMu.Unlock()
	if posted.Get("channel") != "C-slash-completion-test" || posted.Get("thread_ts") != "" {
		t.Fatalf("slash worker posted to channel %q thread %q, want channel-level callback", posted.Get("channel"), posted.Get("thread_ts"))
	}
	var notificationStatus string
	if err := testPool.QueryRow(ctx, `
		SELECT status FROM issue_channel_notification WHERE issue_id = $1
	`, created.ID).Scan(&notificationStatus); err != nil {
		t.Fatalf("load delivered slash notification: %v", err)
	}
	if notificationStatus != "delivered" {
		t.Fatalf("slash notification status = %q, want delivered", notificationStatus)
	}
	var attachedAuditCount int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM issue_channel_notification_audit
		WHERE issue_id = $1 AND created_by_task_id = $2
	`, created.ID, taskID).Scan(&attachedAuditCount); err != nil {
		t.Fatalf("load attached slash notification audit: %v", err)
	}
	if attachedAuditCount != 4 {
		t.Fatalf("attached slash notification audit rows = %d, want target/queue/attempt/delivery", attachedAuditCount)
	}
}
