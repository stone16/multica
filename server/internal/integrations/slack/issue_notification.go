package slack

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/slack-go/slack"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const issueNotificationMetadataType = "multica_issue_notification"
const issueNotificationReconciliationMaxPages = 5

// IssueNotification is the Slack transport boundary for durable issue handoff
// notifications. Database ownership and retry scheduling stay in the worker;
// this type validates the stored target, reconciles an ambiguous previous send,
// and posts one concise message with a stable metadata marker.
type IssueNotification struct {
	q       issueNotificationQueries
	decrypt Decrypter
	appURL  string
	client  func(string) issueNotificationClient
}

type issueNotificationQueries interface {
	GetIssueInWorkspace(context.Context, db.GetIssueInWorkspaceParams) (db.Issue, error)
	GetWorkspace(context.Context, pgtype.UUID) (db.Workspace, error)
	GetChatSessionInWorkspace(context.Context, db.GetChatSessionInWorkspaceParams) (db.ChatSession, error)
	GetChannelChatSessionBindingBySession(context.Context, db.GetChannelChatSessionBindingBySessionParams) (db.ChannelChatSessionBinding, error)
	GetChannelInstallationInWorkspace(context.Context, db.GetChannelInstallationInWorkspaceParams) (db.ChannelInstallation, error)
	GetIssueChannelNotificationHandoff(context.Context, db.GetIssueChannelNotificationHandoffParams) (db.GetIssueChannelNotificationHandoffRow, error)
}

type issueNotificationClient interface {
	PostMessageContext(context.Context, string, ...slack.MsgOption) (string, string, error)
	GetConversationRepliesContext(context.Context, *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error)
	GetConversationHistoryContext(context.Context, *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error)
}

type IssueNotificationResult struct {
	MessageTS string
}

// IssueNotificationOption configures the Slack transport. Production uses the
// default Slack API; tests can point the real slack-go client at httptest.
type IssueNotificationOption func(*IssueNotification)

func WithIssueNotificationAPIURL(apiURL string) IssueNotificationOption {
	return func(notification *IssueNotification) {
		base := strings.TrimRight(strings.TrimSpace(apiURL), "/") + "/"
		notification.client = func(token string) issueNotificationClient {
			return slack.New(token, slack.OptionAPIURL(base))
		}
	}
}

func NewIssueNotification(q issueNotificationQueries, decrypt Decrypter, appURL string, opts ...IssueNotificationOption) *IssueNotification {
	notification := &IssueNotification{
		q: q, decrypt: decrypt, appURL: strings.TrimRight(strings.TrimSpace(appURL), "/"),
		client: func(token string) issueNotificationClient { return slack.New(token) },
	}
	for _, opt := range opts {
		opt(notification)
	}
	return notification
}

func (n *IssueNotification) Send(ctx context.Context, readDB db.DBTX, row db.IssueChannelNotification) (IssueNotificationResult, error) {
	return n.deliver(ctx, readDB, row, true)
}

// Reconcile performs the same target and authorization validation as Send but
// only reads Slack thread metadata. It is used after the final ambiguous POST;
// found=false with nil error is a definitive absence and must never post.
func (n *IssueNotification) Reconcile(ctx context.Context, readDB db.DBTX, row db.IssueChannelNotification) (IssueNotificationResult, bool, error) {
	result, err := n.deliver(ctx, readDB, row, false)
	if err != nil {
		return IssueNotificationResult{}, false, err
	}
	return result, result.MessageTS != "", nil
}

func (n *IssueNotification) deliver(ctx context.Context, readDB db.DBTX, row db.IssueChannelNotification, allowPost bool) (IssueNotificationResult, error) {
	if n == nil || n.q == nil {
		return IssueNotificationResult{}, permanentIssueNotificationError(errors.New("Slack issue notification is not configured"))
	}
	if row.ChannelType != string(TypeSlack) || row.ChannelID == "" || row.ChannelAppID == "" || row.ChannelTeamID == "" {
		return IssueNotificationResult{}, permanentIssueNotificationError(errors.New("invalid Slack issue notification target"))
	}
	queries := n.q
	if readDB != nil {
		queries = db.New(readDB)
	}
	issue, err := queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: row.IssueID, WorkspaceID: row.WorkspaceID})
	if err != nil {
		return IssueNotificationResult{}, classifyIssueNotificationLookupError("load issue", err)
	}
	if issue.Status != "in_review" && issue.Status != "done" {
		return IssueNotificationResult{}, deferIssueNotificationError(fmt.Errorf("issue status is %s, not ready for notification", issue.Status))
	}
	workspace, err := queries.GetWorkspace(ctx, row.WorkspaceID)
	if err != nil {
		return IssueNotificationResult{}, classifyIssueNotificationLookupError("load workspace", err)
	}
	if row.ChatSessionID.Valid {
		if _, err := queries.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{
			ID: row.ChatSessionID, WorkspaceID: row.WorkspaceID,
		}); err != nil {
			return IssueNotificationResult{}, classifyIssueNotificationLookupError("load Slack chat session", err)
		}
		binding, err := queries.GetChannelChatSessionBindingBySession(ctx, db.GetChannelChatSessionBindingBySessionParams{
			ChatSessionID: row.ChatSessionID, ChannelType: string(TypeSlack),
		})
		if err != nil {
			return IssueNotificationResult{}, classifyIssueNotificationLookupError("load Slack chat binding", err)
		}
		currentChannelID, _ := outboundTarget(binding)
		if binding.InstallationID != row.InstallationID || currentChannelID != row.ChannelID {
			return IssueNotificationResult{}, permanentIssueNotificationError(errors.New("Slack issue notification binding no longer matches its stored target"))
		}
	}
	installation, err := queries.GetChannelInstallationInWorkspace(ctx, db.GetChannelInstallationInWorkspaceParams{
		ID: row.InstallationID, WorkspaceID: row.WorkspaceID, ChannelType: string(TypeSlack),
	})
	if err != nil {
		return IssueNotificationResult{}, classifyIssueNotificationLookupError("load Slack installation", err)
	}
	if installation.Status != "active" {
		return IssueNotificationResult{}, permanentIssueNotificationError(errors.New("Slack installation is not active"))
	}
	public := DecodePublicConfig(installation.Config)
	if public.AppID != row.ChannelAppID || public.TeamID != row.ChannelTeamID {
		return IssueNotificationResult{}, permanentIssueNotificationError(errors.New("Slack installation identity no longer matches notification fence"))
	}
	creds, err := decodeCredentials(installation.Config, n.decrypt)
	if err != nil {
		return IssueNotificationResult{}, permanentIssueNotificationError(fmt.Errorf("decode Slack credentials: %w", err))
	}
	if creds.BotToken == "" {
		return IssueNotificationResult{}, permanentIssueNotificationError(errors.New("Slack bot token is empty"))
	}
	client := n.client(creds.BotToken)
	ts := ""
	if !allowPost || row.ClaimCount > 1 {
		var found bool
		var reconcileErr error
		ts, found, reconcileErr = findIssueNotification(ctx, client, row)
		if reconcileErr != nil {
			return IssueNotificationResult{}, classifyIssueNotificationPrePostError(fmt.Errorf("reconcile Slack issue notification: %w", reconcileErr))
		}
		if found {
			return IssueNotificationResult{MessageTS: ts}, nil
		}
	}
	if !allowPost {
		return IssueNotificationResult{}, nil
	}

	identifier := fmt.Sprintf("%s-%d", workspace.IssuePrefix, issue.Number)
	issueLabel := escapeSlackText(identifier)
	issueURL := ""
	if n.appURL != "" {
		issueURL = n.appURL + "/" + url.PathEscape(workspace.Slug) + "/issues/" + url.PathEscape(identifier)
		issueLabel = fmt.Sprintf("<%s|%s>", issueURL, issueLabel)
	}
	verb := "is ready for review"
	if issue.Status == "done" {
		verb = "is complete"
	}
	text := fmt.Sprintf(":white_check_mark: Issue *%s* %s.", issueLabel, verb)
	if handoff, handoffErr := queries.GetIssueChannelNotificationHandoff(ctx, db.GetIssueChannelNotificationHandoffParams{
		IssueID: row.IssueID, WorkspaceID: row.WorkspaceID,
	}); handoffErr == nil && handoff.Kind == "pull_request" {
		text += "\n" + formatIssueNotificationHandoff(handoff)
	} else if handoffErr != nil && !errors.Is(handoffErr, pgx.ErrNoRows) {
		return IssueNotificationResult{}, classifyIssueNotificationLookupError("load issue handoff", handoffErr)
	}
	text += "\nReview the durable evidence in Multica, then reply here if changes are needed."
	metadata := slack.SlackMetadata{
		EventType: issueNotificationMetadataType,
		EventPayload: map[string]any{
			"notification_id": util.UUIDToString(row.IssueID),
			"issue_id":        util.UUIDToString(row.IssueID),
		},
	}
	options := []slack.MsgOption{
		slack.MsgOptionText(text, false),
		slack.MsgOptionDisableLinkUnfurl(),
		slack.MsgOptionMetadata(metadata),
	}
	if row.ThreadTs != "" {
		options = append(options, slack.MsgOptionTS(row.ThreadTs))
	}
	_, ts, err = client.PostMessageContext(ctx, row.ChannelID, options...)
	if err != nil {
		return IssueNotificationResult{}, classifyIssueNotificationError(err)
	}
	return IssueNotificationResult{MessageTS: ts}, nil
}

func formatIssueNotificationHandoff(handoff db.GetIssueChannelNotificationHandoffRow) string {
	state := strings.TrimSpace(handoff.ResultState)
	if state == "" {
		state = "linked"
	}
	return fmt.Sprintf("Result: <%s|pull request> (%s).", handoff.Value, escapeSlackText(state))
}

func findIssueNotification(ctx context.Context, client issueNotificationClient, row db.IssueChannelNotification) (string, bool, error) {
	if row.ThreadTs == "" {
		return findIssueNotificationInChannel(ctx, client, row)
	}
	cursor := ""
	for page := 0; page < issueNotificationReconciliationMaxPages; page++ {
		messages, hasMore, next, err := client.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
			ChannelID: row.ChannelID, Timestamp: row.ThreadTs, Cursor: cursor, Limit: 100,
			Oldest: issueNotificationReconciliationOldest(row), IncludeAllMetadata: true,
		})
		if err != nil {
			return "", false, err
		}
		for _, message := range messages {
			if issueNotificationMatches(message, row.IssueID) {
				return message.Timestamp, true, nil
			}
		}
		if !hasMore || next == "" {
			return "", false, nil
		}
		cursor = next
	}
	return "", false, errors.New("Slack issue notification reconciliation page limit reached")
}

func findIssueNotificationInChannel(ctx context.Context, client issueNotificationClient, row db.IssueChannelNotification) (string, bool, error) {
	cursor := ""
	for page := 0; page < issueNotificationReconciliationMaxPages; page++ {
		response, err := client.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{
			ChannelID: row.ChannelID, Cursor: cursor, Limit: 100,
			Oldest: issueNotificationReconciliationOldest(row), IncludeAllMetadata: true,
		})
		if err != nil {
			return "", false, err
		}
		for _, message := range response.Messages {
			if issueNotificationMatches(message, row.IssueID) {
				return message.Timestamp, true, nil
			}
		}
		cursor = response.ResponseMetaData.NextCursor
		if !response.HasMore || cursor == "" {
			return "", false, nil
		}
	}
	return "", false, errors.New("Slack issue notification reconciliation page limit reached")
}

func issueNotificationReconciliationOldest(row db.IssueChannelNotification) string {
	if !row.CreatedAt.Valid {
		return ""
	}
	return strconv.FormatFloat(float64(row.CreatedAt.Time.UnixNano())/float64(time.Second), 'f', 6, 64)
}

func issueNotificationMatches(message slack.Message, issueID pgtype.UUID) bool {
	return message.Metadata.EventType == issueNotificationMetadataType &&
		fmt.Sprint(message.Metadata.EventPayload["notification_id"]) == util.UUIDToString(issueID)
}

func escapeSlackText(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return replacer.Replace(value)
}

type issueNotificationDeliveryError struct {
	err        error
	permanent  bool
	retryAfter time.Duration
	deferSend  bool
	ambiguous  bool
}

func (e *issueNotificationDeliveryError) Error() string             { return e.err.Error() }
func (e *issueNotificationDeliveryError) Unwrap() error             { return e.err }
func (e *issueNotificationDeliveryError) Permanent() bool           { return e.permanent }
func (e *issueNotificationDeliveryError) RetryAfter() time.Duration { return e.retryAfter }
func (e *issueNotificationDeliveryError) Defer() bool               { return e.deferSend }
func (e *issueNotificationDeliveryError) Ambiguous() bool           { return e.ambiguous }

func permanentIssueNotificationError(err error) error {
	return &issueNotificationDeliveryError{err: err, permanent: true}
}

func deferIssueNotificationError(err error) error {
	return &issueNotificationDeliveryError{err: err, deferSend: true}
}

func classifyIssueNotificationLookupError(operation string, err error) error {
	wrapped := fmt.Errorf("%s: %w", operation, err)
	if errors.Is(err, pgx.ErrNoRows) {
		return permanentIssueNotificationError(wrapped)
	}
	return &issueNotificationDeliveryError{err: wrapped}
}

func classifyIssueNotificationError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &issueNotificationDeliveryError{err: err, ambiguous: true}
	}
	var rateLimited *slack.RateLimitedError
	if errors.As(err, &rateLimited) {
		return &issueNotificationDeliveryError{err: err, retryAfter: rateLimited.RetryAfter}
	}
	var status interface{ HTTPStatusCode() int }
	if errors.As(err, &status) {
		statusCode := status.HTTPStatusCode()
		if statusCode == http.StatusRequestTimeout || statusCode == http.StatusTooManyRequests || statusCode >= 500 {
			return &issueNotificationDeliveryError{err: err, ambiguous: statusCode != http.StatusTooManyRequests}
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return &issueNotificationDeliveryError{err: err, ambiguous: true}
	}
	var apiErr slack.SlackErrorResponse
	if errors.As(err, &apiErr) {
		switch apiErr.Err {
		case "internal_error", "fatal_error", "request_timeout", "service_unavailable":
			return &issueNotificationDeliveryError{err: err}
		}
	}
	return permanentIssueNotificationError(err)
}

func classifyIssueNotificationPrePostError(err error) error {
	classified := classifyIssueNotificationError(err)
	var delivery *issueNotificationDeliveryError
	if errors.As(classified, &delivery) {
		copy := *delivery
		copy.ambiguous = false
		return &copy
	}
	return classified
}
