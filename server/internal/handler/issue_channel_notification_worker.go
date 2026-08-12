package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	channelslack "github.com/multica-ai/multica/server/internal/integrations/slack"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	issueChannelNotificationPollInterval       = time.Second
	issueChannelNotificationConcurrency        = 2
	issueChannelNotificationMaxAttempts        = 5
	issueChannelNotificationMaxReconciliations = 5
	issueChannelNotificationSendTimeout        = 30 * time.Second
)

type issueChannelNotificationSender interface {
	Send(context.Context, db.DBTX, db.IssueChannelNotification) (channelslack.IssueNotificationResult, error)
	Reconcile(context.Context, db.DBTX, db.IssueChannelNotification) (channelslack.IssueNotificationResult, bool, error)
}

type issueChannelNotificationQueries interface {
	ClaimQueuedIssueChannelNotification(context.Context) (db.IssueChannelNotification, error)
	AuditClaimedIssueChannelNotificationAttempt(context.Context, db.AuditClaimedIssueChannelNotificationAttemptParams) (db.AuditClaimedIssueChannelNotificationAttemptRow, error)
	DeferClaimedIssueChannelNotification(context.Context, db.DeferClaimedIssueChannelNotificationParams) (db.IssueChannelNotification, error)
	DeliverClaimedIssueChannelNotification(context.Context, db.DeliverClaimedIssueChannelNotificationParams) (pgtype.UUID, error)
	FailClaimedIssueChannelNotification(context.Context, db.FailClaimedIssueChannelNotificationParams) (pgtype.UUID, error)
	RetryClaimedIssueChannelNotification(context.Context, db.RetryClaimedIssueChannelNotificationParams) (db.IssueChannelNotification, error)
	ReconcileClaimedIssueChannelNotification(context.Context, db.ReconcileClaimedIssueChannelNotificationParams) (db.IssueChannelNotification, error)
	RetryReconcilingIssueChannelNotification(context.Context, db.RetryReconcilingIssueChannelNotificationParams) (db.IssueChannelNotification, error)
	DeliverReconcilingIssueChannelNotification(context.Context, db.DeliverReconcilingIssueChannelNotificationParams) (pgtype.UUID, error)
	FailReconcilingIssueChannelNotification(context.Context, db.FailReconcilingIssueChannelNotificationParams) (pgtype.UUID, error)
}

// IssueChannelNotificationWorker owns the durable issue-to-channel delivery
// queue. PostgreSQL leases distribute work across server replicas and make a
// process crash recoverable; Slack message metadata closes the ambiguous
// accepted-response window on the next attempt.
type IssueChannelNotificationWorker struct {
	queries issueChannelNotificationQueries
	tx      issueChannelNotificationTxStarter
	sender  issueChannelNotificationSender
	notify  chan struct{}
	done    chan struct{}
}

type issueChannelNotificationTxStarter interface {
	Begin(context.Context) (pgx.Tx, error)
}

func NewIssueChannelNotificationWorker(q issueChannelNotificationQueries, tx issueChannelNotificationTxStarter, sender issueChannelNotificationSender) *IssueChannelNotificationWorker {
	return &IssueChannelNotificationWorker{
		queries: q, tx: tx, sender: sender,
		notify: make(chan struct{}, issueChannelNotificationConcurrency),
		done:   make(chan struct{}),
	}
}

func (w *IssueChannelNotificationWorker) Notify() {
	if w == nil {
		return
	}
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

func (w *IssueChannelNotificationWorker) Run(ctx context.Context) {
	if w == nil {
		return
	}
	defer close(w.done)
	if w.queries == nil || w.sender == nil {
		return
	}
	var workers sync.WaitGroup
	workers.Add(issueChannelNotificationConcurrency)
	for range issueChannelNotificationConcurrency {
		go func() {
			defer workers.Done()
			w.runLoop(ctx)
		}()
	}
	workers.Wait()
}

func (w *IssueChannelNotificationWorker) runLoop(ctx context.Context) {
	ticker := time.NewTicker(issueChannelNotificationPollInterval)
	defer ticker.Stop()
	for {
		worked, err := w.ProcessNext(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("issue channel notification worker: process", "error", err)
		}
		if worked {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-w.notify:
		case <-ticker.C:
		}
	}
}

func (w *IssueChannelNotificationWorker) WaitWithTimeout(timeout time.Duration) bool {
	if w == nil {
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-w.done:
		return true
	case <-timer.C:
		return false
	}
}

func (w *IssueChannelNotificationWorker) ProcessNext(ctx context.Context) (bool, error) {
	row, err := w.queries.ClaimQueuedIssueChannelNotification(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim issue channel notification: %w", err)
	}
	if w.tx == nil {
		started, err := w.queries.AuditClaimedIssueChannelNotificationAttempt(ctx, db.AuditClaimedIssueChannelNotificationAttemptParams{
			IssueID: row.IssueID, WorkspaceID: row.WorkspaceID, LeaseToken: row.LeaseToken,
		})
		if err != nil {
			return true, fmt.Errorf("audit issue channel notification attempt: %w", err)
		}
		row = issueChannelNotificationFromAttempt(started)
		if row.Status == "reconciling" {
			return w.reconcileClaimed(ctx, w.queries, nil, row)
		}
		return w.sendClaimed(ctx, w.queries, nil, row)
	}
	tx, err := w.tx.Begin(ctx)
	if err != nil {
		return true, fmt.Errorf("begin issue channel notification delivery: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	qtx := db.New(tx)
	if _, err := qtx.LockWorkspaceForIssueNotification(ctx, row.WorkspaceID); err != nil {
		return true, fmt.Errorf("lock issue notification workspace: %w", err)
	}
	lockedIssue, err := qtx.LockIssueForIssueNotification(ctx, db.LockIssueForIssueNotificationParams{
		ID: row.IssueID, WorkspaceID: row.WorkspaceID,
	})
	if err != nil {
		return true, fmt.Errorf("lock issue notification issue: %w", err)
	}
	if lockedIssue.Status != "in_review" && lockedIssue.Status != "done" {
		if _, err := qtx.DeferClaimedIssueChannelNotification(ctx, db.DeferClaimedIssueChannelNotificationParams{
			IssueID: row.IssueID, WorkspaceID: row.WorkspaceID, LeaseToken: row.LeaseToken,
			AvailableAt: pgtype.Timestamptz{Time: time.Now().Add(issueChannelNotificationPollInterval), Valid: true},
		}); err != nil {
			return true, normalizeIssueChannelNotificationLeaseError("defer reopened", row, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return true, fmt.Errorf("commit reopened issue channel notification defer: %w", err)
		}
		return true, nil
	}
	if row.ChatSessionID.Valid {
		if _, err := qtx.LockChatSessionForIssueNotification(ctx, db.LockChatSessionForIssueNotificationParams{
			ID: row.ChatSessionID, WorkspaceID: row.WorkspaceID,
		}); err != nil {
			return true, fmt.Errorf("lock issue notification chat session: %w", err)
		}
	}
	if _, err := qtx.LockChannelInstallationForIssueNotification(ctx, db.LockChannelInstallationForIssueNotificationParams{
		ID: row.InstallationID, WorkspaceID: row.WorkspaceID, ChannelType: row.ChannelType,
	}); err != nil {
		return true, fmt.Errorf("lock issue notification installation: %w", err)
	}
	locked, err := qtx.LockClaimedIssueChannelNotificationForDelivery(ctx, db.LockClaimedIssueChannelNotificationForDeliveryParams{
		IssueID: row.IssueID, WorkspaceID: row.WorkspaceID, LeaseToken: row.LeaseToken,
	})
	if err != nil {
		return true, normalizeIssueChannelNotificationLeaseError("lock delivery", row, err)
	}
	row = locked
	started, err := qtx.AuditClaimedIssueChannelNotificationAttempt(ctx, db.AuditClaimedIssueChannelNotificationAttemptParams{
		IssueID: row.IssueID, WorkspaceID: row.WorkspaceID, LeaseToken: row.LeaseToken,
	})
	if err != nil {
		return true, fmt.Errorf("audit issue channel notification attempt: %w", err)
	}
	row = issueChannelNotificationFromAttempt(started)
	var worked bool
	if row.Status == "reconciling" {
		worked, err = w.reconcileClaimed(ctx, qtx, tx, row)
	} else {
		worked, err = w.sendClaimed(ctx, qtx, tx, row)
	}
	if err != nil {
		return worked, err
	}
	if err := tx.Commit(ctx); err != nil {
		return true, fmt.Errorf("commit issue channel notification delivery: %w", err)
	}
	return worked, nil
}

func issueChannelNotificationFromAttempt(row db.AuditClaimedIssueChannelNotificationAttemptRow) db.IssueChannelNotification {
	return db.IssueChannelNotification(row)
}

func (w *IssueChannelNotificationWorker) sendClaimed(ctx context.Context, q issueChannelNotificationQueries, readDB db.DBTX, row db.IssueChannelNotification) (bool, error) {
	// Keep every Slack attempt well inside the two-minute database lease. This
	// prevents a second replica from reclaiming the row while the first call can
	// still post, which would otherwise reopen a duplicate-send window.
	deliveryCtx, cancel := context.WithTimeout(ctx, issueChannelNotificationSendTimeout)
	result, sendErr := w.sender.Send(deliveryCtx, readDB, row)
	cancel()
	if sendErr == nil {
		_, err := q.DeliverClaimedIssueChannelNotification(ctx, db.DeliverClaimedIssueChannelNotificationParams{
			IssueID: row.IssueID, WorkspaceID: row.WorkspaceID, LeaseToken: row.LeaseToken,
			SlackMessageTs: pgtype.Text{String: result.MessageTS, Valid: result.MessageTS != ""},
		})
		return true, normalizeIssueChannelNotificationLeaseError("deliver", row, err)
	}
	// Shutdown cancellation is not a delivery outcome. Leave the claim leased so
	// another worker can reconcile the Slack thread after the lease expires;
	// marking it failed here would turn a normal process stop into dead-letter.
	if ctx.Err() != nil {
		return true, ctx.Err()
	}

	var classified interface {
		Permanent() bool
		RetryAfter() time.Duration
		Defer() bool
	}
	isClassified := errors.As(sendErr, &classified)
	if isClassified && classified.Defer() {
		_, err := q.DeferClaimedIssueChannelNotification(ctx, db.DeferClaimedIssueChannelNotificationParams{
			IssueID: row.IssueID, WorkspaceID: row.WorkspaceID, LeaseToken: row.LeaseToken,
			AvailableAt: pgtype.Timestamptz{Time: time.Now().Add(issueChannelNotificationPollInterval), Valid: true},
		})
		return true, normalizeIssueChannelNotificationLeaseError("defer", row, err)
	}
	permanent := isClassified && classified.Permanent()
	ambiguous := true
	var ambiguity interface{ Ambiguous() bool }
	if errors.As(sendErr, &ambiguity) {
		ambiguous = ambiguity.Ambiguous()
	}
	if permanent || (row.Attempts >= issueChannelNotificationMaxAttempts && !ambiguous) {
		_, err := q.FailClaimedIssueChannelNotification(ctx, db.FailClaimedIssueChannelNotificationParams{
			IssueID: row.IssueID, WorkspaceID: row.WorkspaceID, LeaseToken: row.LeaseToken,
			LastError: pgtype.Text{String: sendErr.Error(), Valid: true},
		})
		if err == nil {
			slog.Error("issue channel notification dead-lettered",
				"issue_id", uuidToString(row.IssueID),
				"attempts", row.Attempts,
				"error", sendErr,
			)
		}
		return true, normalizeIssueChannelNotificationLeaseError("fail", row, err)
	}
	if row.Attempts >= issueChannelNotificationMaxAttempts {
		_, err := q.ReconcileClaimedIssueChannelNotification(ctx, db.ReconcileClaimedIssueChannelNotificationParams{
			IssueID: row.IssueID, WorkspaceID: row.WorkspaceID, LeaseToken: row.LeaseToken,
			AvailableAt: pgtype.Timestamptz{Time: time.Now().Add(issueChannelNotificationPollInterval), Valid: true},
			LastError:   pgtype.Text{String: sendErr.Error(), Valid: true},
		})
		return true, normalizeIssueChannelNotificationLeaseError("enter reconciliation", row, err)
	}

	backoff := time.Second * time.Duration(1<<min(row.Attempts-1, 6))
	if isClassified && classified.RetryAfter() > 0 {
		backoff = classified.RetryAfter()
	} else {
		// Add bounded jitter to keep replicas that failed together from retrying
		// the same Slack channel in lockstep.
		jitterCeiling := min(backoff/4, time.Second)
		if jitterCeiling > 0 {
			backoff += time.Duration(rand.Int64N(int64(jitterCeiling)))
		}
	}
	_, err := q.RetryClaimedIssueChannelNotification(ctx, db.RetryClaimedIssueChannelNotificationParams{
		IssueID: row.IssueID, WorkspaceID: row.WorkspaceID, LeaseToken: row.LeaseToken,
		AvailableAt: pgtype.Timestamptz{Time: time.Now().Add(backoff), Valid: true},
		LastError:   pgtype.Text{String: sendErr.Error(), Valid: true},
	})
	return true, normalizeIssueChannelNotificationLeaseError("retry", row, err)
}

func (w *IssueChannelNotificationWorker) reconcileClaimed(ctx context.Context, q issueChannelNotificationQueries, readDB db.DBTX, row db.IssueChannelNotification) (bool, error) {
	deliveryCtx, cancel := context.WithTimeout(ctx, issueChannelNotificationSendTimeout)
	result, found, reconcileErr := w.sender.Reconcile(deliveryCtx, readDB, row)
	cancel()
	if reconcileErr == nil && found {
		_, err := q.DeliverReconcilingIssueChannelNotification(ctx, db.DeliverReconcilingIssueChannelNotificationParams{
			IssueID: row.IssueID, WorkspaceID: row.WorkspaceID, LeaseToken: row.LeaseToken,
			SlackMessageTs: pgtype.Text{String: result.MessageTS, Valid: result.MessageTS != ""},
		})
		return true, normalizeIssueChannelNotificationLeaseError("deliver reconciled", row, err)
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	if reconcileErr == nil {
		_, err := q.FailReconcilingIssueChannelNotification(ctx, db.FailReconcilingIssueChannelNotificationParams{
			IssueID: row.IssueID, WorkspaceID: row.WorkspaceID, LeaseToken: row.LeaseToken,
			LastError: pgtype.Text{String: "Slack metadata reconciliation confirmed no delivery", Valid: true},
		})
		return true, normalizeIssueChannelNotificationLeaseError("fail reconciled", row, err)
	}
	var classified interface {
		Permanent() bool
		RetryAfter() time.Duration
		Defer() bool
	}
	isClassified := errors.As(reconcileErr, &classified)
	if isClassified && classified.Defer() {
		_, err := q.DeferClaimedIssueChannelNotification(ctx, db.DeferClaimedIssueChannelNotificationParams{
			IssueID: row.IssueID, WorkspaceID: row.WorkspaceID, LeaseToken: row.LeaseToken,
			AvailableAt: pgtype.Timestamptz{Time: time.Now().Add(issueChannelNotificationPollInterval), Valid: true},
		})
		return true, normalizeIssueChannelNotificationLeaseError("defer reconciliation", row, err)
	}
	if isClassified && classified.Permanent() {
		_, err := q.FailReconcilingIssueChannelNotification(ctx, db.FailReconcilingIssueChannelNotificationParams{
			IssueID: row.IssueID, WorkspaceID: row.WorkspaceID, LeaseToken: row.LeaseToken,
			LastError: pgtype.Text{String: reconcileErr.Error(), Valid: true},
		})
		return true, normalizeIssueChannelNotificationLeaseError("fail reconciliation", row, err)
	}
	if row.ReconciliationAttempts >= issueChannelNotificationMaxReconciliations {
		_, err := q.FailReconcilingIssueChannelNotification(ctx, db.FailReconcilingIssueChannelNotificationParams{
			IssueID: row.IssueID, WorkspaceID: row.WorkspaceID, LeaseToken: row.LeaseToken,
			LastError: pgtype.Text{String: reconcileErr.Error(), Valid: true},
		})
		return true, normalizeIssueChannelNotificationLeaseError("fail reconciliation", row, err)
	}
	backoff := time.Second * time.Duration(1<<min(row.ReconciliationAttempts-1, 6))
	if isClassified && classified.RetryAfter() > 0 {
		backoff = classified.RetryAfter()
	}
	_, err := q.RetryReconcilingIssueChannelNotification(ctx, db.RetryReconcilingIssueChannelNotificationParams{
		IssueID: row.IssueID, WorkspaceID: row.WorkspaceID, LeaseToken: row.LeaseToken,
		AvailableAt: pgtype.Timestamptz{Time: time.Now().Add(backoff), Valid: true},
		LastError:   pgtype.Text{String: reconcileErr.Error(), Valid: true},
	})
	return true, normalizeIssueChannelNotificationLeaseError("retry reconciliation", row, err)
}

func normalizeIssueChannelNotificationLeaseError(operation string, row db.IssueChannelNotification, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s issue channel notification %s: %w", operation, uuidToString(row.IssueID), err)
}
