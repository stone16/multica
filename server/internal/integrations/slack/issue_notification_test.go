package slack

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/slack-go/slack"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type fakeIssueNotificationClient struct {
	messages     []slack.Message
	replyPages   [][]slack.Message
	historyPages [][]slack.Message
	replyCalls   int
	historyCalls int
	postTS       string
	postErr      error
	gotForm      url.Values
}

type fakeIssueNotificationQueries struct {
	issue        db.Issue
	workspace    db.Workspace
	session      db.ChatSession
	binding      db.ChannelChatSessionBinding
	installation db.ChannelInstallation
	issueErr     error
	handoff      db.GetIssueChannelNotificationHandoffRow
	handoffErr   error
}

func (f fakeIssueNotificationQueries) GetIssueInWorkspace(context.Context, db.GetIssueInWorkspaceParams) (db.Issue, error) {
	return f.issue, f.issueErr
}
func (f fakeIssueNotificationQueries) GetWorkspace(context.Context, pgtype.UUID) (db.Workspace, error) {
	return f.workspace, nil
}
func (f fakeIssueNotificationQueries) GetChatSessionInWorkspace(context.Context, db.GetChatSessionInWorkspaceParams) (db.ChatSession, error) {
	return f.session, nil
}
func (f fakeIssueNotificationQueries) GetChannelChatSessionBindingBySession(context.Context, db.GetChannelChatSessionBindingBySessionParams) (db.ChannelChatSessionBinding, error) {
	return f.binding, nil
}
func (f fakeIssueNotificationQueries) GetChannelInstallationInWorkspace(context.Context, db.GetChannelInstallationInWorkspaceParams) (db.ChannelInstallation, error) {
	return f.installation, nil
}
func (f fakeIssueNotificationQueries) GetIssueChannelNotificationHandoff(context.Context, db.GetIssueChannelNotificationHandoffParams) (db.GetIssueChannelNotificationHandoffRow, error) {
	if f.handoffErr != nil {
		return db.GetIssueChannelNotificationHandoffRow{}, f.handoffErr
	}
	if f.handoff.Kind == "" {
		return db.GetIssueChannelNotificationHandoffRow{}, pgx.ErrNoRows
	}
	return f.handoff, nil
}

func (f *fakeIssueNotificationClient) GetConversationRepliesContext(_ context.Context, params *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error) {
	page := f.replyCalls
	f.replyCalls++
	if len(f.replyPages) == 0 {
		return f.messages, false, "", nil
	}
	if page >= len(f.replyPages) {
		return nil, false, "", nil
	}
	hasMore := page+1 < len(f.replyPages)
	next := ""
	if hasMore {
		next = "reply-page-" + string(rune('2'+page))
	}
	return f.replyPages[page], hasMore, next, nil
}

func (f *fakeIssueNotificationClient) GetConversationHistoryContext(_ context.Context, params *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error) {
	page := f.historyCalls
	f.historyCalls++
	if len(f.historyPages) == 0 {
		return &slack.GetConversationHistoryResponse{Messages: f.messages}, nil
	}
	if page >= len(f.historyPages) {
		return &slack.GetConversationHistoryResponse{}, nil
	}
	hasMore := page+1 < len(f.historyPages)
	next := ""
	if hasMore {
		next = "history-page-" + string(rune('2'+page))
	}
	response := &slack.GetConversationHistoryResponse{Messages: f.historyPages[page], HasMore: hasMore}
	response.ResponseMetaData.NextCursor = next
	return response, nil
}

func (f *fakeIssueNotificationClient) PostMessageContext(_ context.Context, channel string, options ...slack.MsgOption) (string, string, error) {
	if f.postErr != nil {
		return "", "", f.postErr
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C1","ts":"1.1"}`))
	}))
	defer server.Close()
	api := slack.New("xoxb-test", slack.OptionAPIURL(server.URL+"/"))
	return api.PostMessageContext(context.Background(), channel, options...)
}

func TestFindIssueNotification_ReplayFindsMetadataMarker(t *testing.T) {
	issueID := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	client := &fakeIssueNotificationClient{messages: []slack.Message{{Msg: slack.Msg{
		Timestamp: "1700000000.9",
		Metadata: slack.SlackMetadata{
			EventType:    issueNotificationMetadataType,
			EventPayload: map[string]any{"notification_id": "01000000-0000-0000-0000-000000000000"},
		},
	}}}}
	ts, found, err := findIssueNotification(context.Background(), client, db.IssueChannelNotification{
		IssueID: issueID, ChannelID: "C1", ThreadTs: "1700000000.1",
	})
	if err != nil || !found || ts != "1700000000.9" {
		t.Fatalf("findIssueNotification = (%q, %v, %v), want existing marker", ts, found, err)
	}
}

func TestFindIssueNotification_SlashReplayFindsChannelMetadataMarker(t *testing.T) {
	issueID := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	client := &fakeIssueNotificationClient{messages: []slack.Message{{Msg: slack.Msg{
		Timestamp: "1700000000.9",
		Metadata: slack.SlackMetadata{
			EventType:    issueNotificationMetadataType,
			EventPayload: map[string]any{"notification_id": "01000000-0000-0000-0000-000000000000"},
		},
	}}}}
	ts, found, err := findIssueNotification(context.Background(), client, db.IssueChannelNotification{
		IssueID: issueID, ChannelID: "C1",
	})
	if err != nil || !found || ts != "1700000000.9" {
		t.Fatalf("find slash issue notification = (%q, %v, %v), want existing channel marker", ts, found, err)
	}
}

func TestFindIssueNotification_FindsMarkerOnLaterBoundedPage(t *testing.T) {
	issueID := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	client := &fakeIssueNotificationClient{replyPages: [][]slack.Message{
		{{Msg: slack.Msg{Timestamp: "1700000000.1"}}},
		{{Msg: slack.Msg{Timestamp: "1700000000.2"}}},
		{{Msg: slack.Msg{
			Timestamp: "1700000000.9",
			Metadata: slack.SlackMetadata{
				EventType:    issueNotificationMetadataType,
				EventPayload: map[string]any{"notification_id": "01000000-0000-0000-0000-000000000000"},
			},
		}}},
	}}
	ts, found, err := findIssueNotification(context.Background(), client, db.IssueChannelNotification{
		IssueID: issueID, ChannelID: "C1", ThreadTs: "1700000000.1",
		CreatedAt: pgtype.Timestamptz{Time: time.Unix(1_700_000_000, 0), Valid: true},
	})
	if err != nil || !found || ts != "1700000000.9" {
		t.Fatalf("find later-page issue notification = (%q, %v, %v)", ts, found, err)
	}
	if client.replyCalls != 3 {
		t.Fatalf("reply pages read = %d, want 3", client.replyCalls)
	}
}

func TestFindIssueNotification_ChannelFindsMarkerOnLaterBoundedPage(t *testing.T) {
	issueID := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	client := &fakeIssueNotificationClient{historyPages: [][]slack.Message{
		{{Msg: slack.Msg{Timestamp: "1700000000.1"}}},
		{{Msg: slack.Msg{Timestamp: "1700000000.2"}}},
		{{Msg: slack.Msg{
			Timestamp: "1700000000.9",
			Metadata: slack.SlackMetadata{
				EventType:    issueNotificationMetadataType,
				EventPayload: map[string]any{"notification_id": "01000000-0000-0000-0000-000000000000"},
			},
		}}},
	}}
	ts, found, err := findIssueNotification(context.Background(), client, db.IssueChannelNotification{
		IssueID: issueID, ChannelID: "C1",
		CreatedAt: pgtype.Timestamptz{Time: time.Unix(1_700_000_000, 0), Valid: true},
	})
	if err != nil || !found || ts != "1700000000.9" {
		t.Fatalf("find later-page channel issue notification = (%q, %v, %v)", ts, found, err)
	}
	if client.historyCalls != 3 {
		t.Fatalf("history pages read = %d, want 3", client.historyCalls)
	}
}

func TestFindIssueNotification_BoundsBusyThreadReconciliation(t *testing.T) {
	pages := make([][]slack.Message, issueNotificationReconciliationMaxPages+1)
	for i := range pages {
		pages[i] = []slack.Message{{Msg: slack.Msg{Timestamp: "1700000000.1"}}}
	}
	client := &fakeIssueNotificationClient{replyPages: pages}
	_, found, err := findIssueNotification(context.Background(), client, db.IssueChannelNotification{
		IssueID: pgtype.UUID{Bytes: [16]byte{1}, Valid: true}, ChannelID: "C1", ThreadTs: "1700000000.1",
		CreatedAt: pgtype.Timestamptz{Time: time.Unix(1_700_000_000, 0), Valid: true},
	})
	if err == nil || found {
		t.Fatalf("busy thread reconciliation = (found %v, err %v), want bounded inconclusive error", found, err)
	}
	if client.replyCalls != issueNotificationReconciliationMaxPages {
		t.Fatalf("reply pages read = %d, want bound %d", client.replyCalls, issueNotificationReconciliationMaxPages)
	}
}

func TestFindIssueNotification_BoundsBusyChannelReconciliation(t *testing.T) {
	pages := make([][]slack.Message, issueNotificationReconciliationMaxPages+1)
	for i := range pages {
		pages[i] = []slack.Message{{Msg: slack.Msg{Timestamp: "1700000000.1"}}}
	}
	client := &fakeIssueNotificationClient{historyPages: pages}
	_, found, err := findIssueNotification(context.Background(), client, db.IssueChannelNotification{
		IssueID: pgtype.UUID{Bytes: [16]byte{1}, Valid: true}, ChannelID: "C1",
		CreatedAt: pgtype.Timestamptz{Time: time.Unix(1_700_000_000, 0), Valid: true},
	})
	if err == nil || found {
		t.Fatalf("busy channel reconciliation = (found %v, err %v), want bounded inconclusive error", found, err)
	}
	if client.historyCalls != issueNotificationReconciliationMaxPages {
		t.Fatalf("history pages read = %d, want bound %d", client.historyCalls, issueNotificationReconciliationMaxPages)
	}
}

func TestIssueNotificationErrorClassification(t *testing.T) {
	rateLimited := classifyIssueNotificationError(&slack.RateLimitedError{RetryAfter: 13 * time.Second})
	var classified interface {
		Permanent() bool
		RetryAfter() time.Duration
		Defer() bool
	}
	if !errors.As(rateLimited, &classified) || classified.Permanent() || classified.RetryAfter() != 13*time.Second {
		t.Fatalf("429 classification = %#v", rateLimited)
	}
	rawRateLimited := classifyIssueNotificationError(slack.StatusCodeError{Code: http.StatusTooManyRequests, Status: "429 Too Many Requests"})
	if !errors.As(rawRateLimited, &classified) || classified.Permanent() || classified.RetryAfter() != 0 {
		t.Fatalf("raw HTTP 429 classification = %#v", rawRateLimited)
	}

	permanent := classifyIssueNotificationError(slack.SlackErrorResponse{Err: "channel_not_found"})
	if !errors.As(permanent, &classified) || !classified.Permanent() {
		t.Fatalf("Slack API permanent classification = %#v", permanent)
	}

	transient := classifyIssueNotificationError(slack.StatusCodeError{Code: http.StatusBadGateway, Status: "502 Bad Gateway"})
	if !errors.As(transient, &classified) || classified.Permanent() {
		t.Fatalf("5xx classification = %#v", transient)
	}
	transientAPI := classifyIssueNotificationError(slack.SlackErrorResponse{Err: "internal_error"})
	if !errors.As(transientAPI, &classified) || classified.Permanent() {
		t.Fatalf("Slack transient API classification = %#v", transientAPI)
	}

	timeout := classifyIssueNotificationError(context.DeadlineExceeded)
	if !errors.As(timeout, &classified) || classified.Permanent() {
		t.Fatalf("timeout classification = %#v", timeout)
	}

	dbTransient := classifyIssueNotificationLookupError("load issue", errors.New("database unavailable"))
	if !errors.As(dbTransient, &classified) || classified.Permanent() {
		t.Fatalf("database transient classification = %#v", dbTransient)
	}
	dbMissing := classifyIssueNotificationLookupError("load issue", pgx.ErrNoRows)
	if !errors.As(dbMissing, &classified) || !classified.Permanent() {
		t.Fatalf("database missing-row classification = %#v", dbMissing)
	}

	reopened := deferIssueNotificationError(errors.New("issue reopened"))
	if !errors.As(reopened, &classified) || !classified.Defer() || classified.Permanent() {
		t.Fatalf("reopen classification = %#v", reopened)
	}
}

func TestIssueNotification_SendPostsOriginalThreadWithMetadata(t *testing.T) {
	issueID := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	workspaceID := pgtype.UUID{Bytes: [16]byte{2}, Valid: true}
	sessionID := pgtype.UUID{Bytes: [16]byte{3}, Valid: true}
	installationID := pgtype.UUID{Bytes: [16]byte{4}, Valid: true}
	client := &fakeIssueNotificationClient{}
	notifier := NewIssueNotification(fakeIssueNotificationQueries{
		issue:     db.Issue{ID: issueID, Title: "Customer jane@example.com trace payload", Status: "in_review", Number: 42},
		workspace: db.Workspace{IssuePrefix: "CRM", Slug: "support team"},
		session:   db.ChatSession{ID: sessionID, WorkspaceID: workspaceID},
		binding: db.ChannelChatSessionBinding{
			ChatSessionID: sessionID, InstallationID: installationID, ChannelType: string(TypeSlack),
			ChannelChatID: "C1:1700000000.1", LastThreadID: pgtype.Text{String: "1700000000.1", Valid: true},
			Config: []byte(`{"channel_id":"C1"}`),
		},
		installation: db.ChannelInstallation{
			ID: installationID, WorkspaceID: workspaceID, Status: "active",
			Config: []byte(`{"app_id":"A1","team_id":"T1","bot_token_encrypted":"eG94Yi10ZXN0"}`),
		},
		handoff: db.GetIssueChannelNotificationHandoffRow{Kind: "comment", Value: "Implemented the callback and verified the durable worker."},
	}, nil, "https://app.multica.test/")
	notifier.client = func(string) issueNotificationClient { return client }

	result, err := notifier.Send(context.Background(), nil, db.IssueChannelNotification{
		IssueID: issueID, WorkspaceID: workspaceID, ChatSessionID: sessionID, InstallationID: installationID,
		ChannelType: string(TypeSlack), ChannelAppID: "A1", ChannelTeamID: "T1", ChannelID: "C1", ThreadTs: "1700000000.1",
	})
	if err != nil || result.MessageTS != "1.1" {
		t.Fatalf("Send = (%#v, %v)", result, err)
	}
	if client.gotForm.Get("channel") != "C1" || client.gotForm.Get("thread_ts") != "1700000000.1" {
		t.Fatalf("posted target = channel %q thread %q", client.gotForm.Get("channel"), client.gotForm.Get("thread_ts"))
	}
	if got := client.gotForm.Get("text"); got != ":white_check_mark: Issue *<https://app.multica.test/support%20team/issues/CRM-42|CRM-42>* is ready for review.\nReview the durable evidence in Multica, then reply here if changes are needed." {
		t.Fatalf("posted text = %q", got)
	}
	if got := client.gotForm.Get("text"); strings.Contains(got, "jane@example.com") {
		t.Fatalf("posted text copied private issue title: %q", got)
	}
	if got := client.gotForm.Get("metadata"); got != `{"event_type":"multica_issue_notification","event_payload":{"issue_id":"01000000-0000-0000-0000-000000000000","notification_id":"01000000-0000-0000-0000-000000000000"}}` {
		t.Fatalf("posted metadata = %q", got)
	}
	if client.replyCalls != 0 || client.historyCalls != 0 {
		t.Fatalf("first delivery scanned Slack history: replies=%d history=%d", client.replyCalls, client.historyCalls)
	}
}

func TestIssueNotification_SendIncludesLinkedPullRequestOnly(t *testing.T) {
	issueID := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	workspaceID := pgtype.UUID{Bytes: [16]byte{2}, Valid: true}
	installationID := pgtype.UUID{Bytes: [16]byte{4}, Valid: true}
	client := &fakeIssueNotificationClient{}
	notifier := NewIssueNotification(fakeIssueNotificationQueries{
		issue:     db.Issue{ID: issueID, Status: "in_review", Number: 42},
		workspace: db.Workspace{IssuePrefix: "CRM", Slug: "support"},
		installation: db.ChannelInstallation{
			ID: installationID, WorkspaceID: workspaceID, Status: "active",
			Config: []byte(`{"app_id":"A1","team_id":"T1","bot_token_encrypted":"eG94Yi10ZXN0"}`),
		},
		handoff: db.GetIssueChannelNotificationHandoffRow{
			Kind: "pull_request", Value: "https://github.com/multica-ai/multica/pull/42", ResultState: "open",
		},
	}, nil, "https://app.multica.test")
	notifier.client = func(string) issueNotificationClient { return client }

	_, err := notifier.Send(context.Background(), nil, db.IssueChannelNotification{
		IssueID: issueID, WorkspaceID: workspaceID, InstallationID: installationID,
		ChannelType: string(TypeSlack), ChannelAppID: "A1", ChannelTeamID: "T1", ChannelID: "C1",
	})
	if err != nil {
		t.Fatalf("Send linked pull request: %v", err)
	}
	if got := client.gotForm.Get("text"); !strings.Contains(got, "Result: <https://github.com/multica-ai/multica/pull/42|pull request> (open).") {
		t.Fatalf("linked pull-request completion text = %q", got)
	}
}

func TestIssueNotification_ReconcileOnlyReadsMetadata(t *testing.T) {
	issueID := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	workspaceID := pgtype.UUID{Bytes: [16]byte{2}, Valid: true}
	sessionID := pgtype.UUID{Bytes: [16]byte{3}, Valid: true}
	installationID := pgtype.UUID{Bytes: [16]byte{4}, Valid: true}
	client := &fakeIssueNotificationClient{}
	notifier := NewIssueNotification(fakeIssueNotificationQueries{
		issue: db.Issue{ID: issueID, Status: "in_review"}, workspace: db.Workspace{},
		session: db.ChatSession{ID: sessionID, WorkspaceID: workspaceID},
		binding: db.ChannelChatSessionBinding{
			ChatSessionID: sessionID, InstallationID: installationID, ChannelType: string(TypeSlack),
			ChannelChatID: "C1:1700000000.1", Config: []byte(`{"channel_id":"C1"}`),
		},
		installation: db.ChannelInstallation{
			ID: installationID, WorkspaceID: workspaceID, Status: "active",
			Config: []byte(`{"app_id":"A1","team_id":"T1","bot_token_encrypted":"eG94Yi10ZXN0"}`),
		},
	}, nil, "")
	notifier.client = func(string) issueNotificationClient { return client }

	result, found, err := notifier.Reconcile(context.Background(), nil, db.IssueChannelNotification{
		IssueID: issueID, WorkspaceID: workspaceID, ChatSessionID: sessionID, InstallationID: installationID,
		ChannelType: string(TypeSlack), ChannelAppID: "A1", ChannelTeamID: "T1", ChannelID: "C1", ThreadTs: "1700000000.1",
	})
	if err != nil || found || result.MessageTS != "" {
		t.Fatalf("Reconcile = (%#v, %v, %v), want definitive absence", result, found, err)
	}
	if client.gotForm != nil {
		t.Fatalf("read-only reconciliation reached chat.postMessage: %v", client.gotForm)
	}
}

func TestIssueNotification_SendRejectsChangedBinding(t *testing.T) {
	issueID := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	workspaceID := pgtype.UUID{Bytes: [16]byte{2}, Valid: true}
	sessionID := pgtype.UUID{Bytes: [16]byte{3}, Valid: true}
	installationID := pgtype.UUID{Bytes: [16]byte{4}, Valid: true}
	client := &fakeIssueNotificationClient{}
	notifier := NewIssueNotification(fakeIssueNotificationQueries{
		issue: db.Issue{ID: issueID, Status: "in_review"}, workspace: db.Workspace{},
		session: db.ChatSession{ID: sessionID, WorkspaceID: workspaceID},
		binding: db.ChannelChatSessionBinding{
			ChatSessionID: sessionID, InstallationID: installationID, ChannelType: string(TypeSlack),
			ChannelChatID: "C2:1700000000.1", LastThreadID: pgtype.Text{String: "1700000000.1", Valid: true},
			Config: []byte(`{"channel_id":"C2"}`),
		},
	}, nil, "")
	notifier.client = func(string) issueNotificationClient { return client }

	_, err := notifier.Send(context.Background(), nil, db.IssueChannelNotification{
		IssueID: issueID, WorkspaceID: workspaceID, ChatSessionID: sessionID, InstallationID: installationID,
		ChannelType: string(TypeSlack), ChannelAppID: "A1", ChannelTeamID: "T1", ChannelID: "C1", ThreadTs: "1700000000.1",
	})
	var classified interface{ Permanent() bool }
	if !errors.As(err, &classified) || !classified.Permanent() {
		t.Fatalf("changed binding error = %v, want permanent refusal", err)
	}
	if client.gotForm != nil {
		t.Fatal("changed binding reached Slack")
	}
}

func TestIssueNotification_SendRejectsChangedInstallationIdentity(t *testing.T) {
	issueID := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	workspaceID := pgtype.UUID{Bytes: [16]byte{2}, Valid: true}
	installationID := pgtype.UUID{Bytes: [16]byte{4}, Valid: true}
	client := &fakeIssueNotificationClient{}
	notifier := NewIssueNotification(fakeIssueNotificationQueries{
		issue:     db.Issue{ID: issueID, Status: "in_review"},
		workspace: db.Workspace{},
		installation: db.ChannelInstallation{
			ID: installationID, WorkspaceID: workspaceID, Status: "active",
			Config: []byte(`{"app_id":"A-new","team_id":"T-new","bot_token_encrypted":"eG94Yi10ZXN0"}`),
		},
	}, nil, "")
	notifier.client = func(string) issueNotificationClient { return client }

	_, err := notifier.Send(context.Background(), nil, db.IssueChannelNotification{
		IssueID: issueID, WorkspaceID: workspaceID, InstallationID: installationID,
		ChannelType: string(TypeSlack), ChannelAppID: "A-old", ChannelTeamID: "T-old", ChannelID: "C1",
	})
	var classified interface{ Permanent() bool }
	if !errors.As(err, &classified) || !classified.Permanent() {
		t.Fatalf("changed installation identity error = %v, want permanent refusal", err)
	}
	if client.gotForm != nil {
		t.Fatal("changed installation identity reached Slack")
	}
}

func TestIssueNotification_SendPostsSlashCallbackAtChannelLevel(t *testing.T) {
	issueID := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	workspaceID := pgtype.UUID{Bytes: [16]byte{2}, Valid: true}
	installationID := pgtype.UUID{Bytes: [16]byte{4}, Valid: true}
	client := &fakeIssueNotificationClient{}
	notifier := NewIssueNotification(fakeIssueNotificationQueries{
		issue:     db.Issue{ID: issueID, Status: "done", Number: 7},
		workspace: db.Workspace{IssuePrefix: "MUL", Slug: "team"},
		installation: db.ChannelInstallation{
			ID: installationID, WorkspaceID: workspaceID, Status: "active",
			Config: []byte(`{"app_id":"A1","team_id":"T1","bot_token_encrypted":"eG94Yi10ZXN0"}`),
		},
	}, nil, "https://app.multica.test")
	notifier.client = func(string) issueNotificationClient { return client }

	result, err := notifier.Send(context.Background(), nil, db.IssueChannelNotification{
		IssueID: issueID, WorkspaceID: workspaceID, InstallationID: installationID,
		ChannelType: string(TypeSlack), ChannelAppID: "A1", ChannelTeamID: "T1", ChannelID: "C-slash",
	})
	if err != nil || result.MessageTS != "1.1" {
		t.Fatalf("Send slash callback = (%#v, %v)", result, err)
	}
	if got := client.gotForm.Get("channel"); got != "C-slash" {
		t.Fatalf("slash callback channel = %q, want C-slash", got)
	}
	if got := client.gotForm.Get("thread_ts"); got != "" {
		t.Fatalf("slash callback fabricated thread_ts = %q", got)
	}
}

func TestIssueNotification_HandoffFormatsPullRequest(t *testing.T) {
	pr := formatIssueNotificationHandoff(db.GetIssueChannelNotificationHandoffRow{
		Kind: "pull_request", Value: "https://github.com/multica-ai/multica/pull/42", ResultState: "open",
	})
	if pr != "Result: <https://github.com/multica-ai/multica/pull/42|pull request> (open)." {
		t.Fatalf("pull-request handoff = %q", pr)
	}
}
