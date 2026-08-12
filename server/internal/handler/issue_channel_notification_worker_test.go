package handler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	channelslack "github.com/multica-ai/multica/server/internal/integrations/slack"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type fakeIssueChannelNotificationQueries struct {
	row                db.IssueChannelNotification
	retried            db.RetryClaimedIssueChannelNotificationParams
	deferred           db.DeferClaimedIssueChannelNotificationParams
	reconcile          db.ReconcileClaimedIssueChannelNotificationParams
	retryReconcile     db.RetryReconcilingIssueChannelNotificationParams
	delivered          db.DeliverClaimedIssueChannelNotificationParams
	deliveredReconcile db.DeliverReconcilingIssueChannelNotificationParams
	failed             db.FailClaimedIssueChannelNotificationParams
	failedReconcile    db.FailReconcilingIssueChannelNotificationParams
	audited            db.AuditClaimedIssueChannelNotificationAttemptParams
	auditErr           error
}

func (f *fakeIssueChannelNotificationQueries) ClaimQueuedIssueChannelNotification(context.Context) (db.IssueChannelNotification, error) {
	f.row.ClaimCount++
	return f.row, nil
}
func (f *fakeIssueChannelNotificationQueries) AuditClaimedIssueChannelNotificationAttempt(_ context.Context, p db.AuditClaimedIssueChannelNotificationAttemptParams) (db.AuditClaimedIssueChannelNotificationAttemptRow, error) {
	f.audited = p
	if f.auditErr != nil {
		return db.AuditClaimedIssueChannelNotificationAttemptRow{}, f.auditErr
	}
	if f.row.Status == "reconciling" {
		f.row.ReconciliationAttempts++
	} else {
		f.row.Attempts++
	}
	return db.AuditClaimedIssueChannelNotificationAttemptRow{
		IssueID: f.row.IssueID, WorkspaceID: f.row.WorkspaceID, CreatedByTaskID: f.row.CreatedByTaskID,
		InitiatorUserID: f.row.InitiatorUserID, ChatSessionID: f.row.ChatSessionID, InstallationID: f.row.InstallationID,
		ChannelType: f.row.ChannelType, ChannelAppID: f.row.ChannelAppID, ChannelTeamID: f.row.ChannelTeamID,
		ChannelID: f.row.ChannelID, ThreadTs: f.row.ThreadTs, Status: f.row.Status, AvailableAt: f.row.AvailableAt,
		LeaseToken: f.row.LeaseToken, LeaseExpiresAt: f.row.LeaseExpiresAt, ClaimCount: f.row.ClaimCount, Attempts: f.row.Attempts,
		ReconciliationAttempts: f.row.ReconciliationAttempts, LastError: f.row.LastError,
		SlackMessageTs: f.row.SlackMessageTs, DeliveredAt: f.row.DeliveredAt, FailedAt: f.row.FailedAt,
		CreatedAt: f.row.CreatedAt, QueuedAt: f.row.QueuedAt, CancelledAt: f.row.CancelledAt, UpdatedAt: f.row.UpdatedAt,
	}, nil
}
func (f *fakeIssueChannelNotificationQueries) DeferClaimedIssueChannelNotification(_ context.Context, p db.DeferClaimedIssueChannelNotificationParams) (db.IssueChannelNotification, error) {
	f.deferred = p
	return f.row, nil
}
func (f *fakeIssueChannelNotificationQueries) DeliverClaimedIssueChannelNotification(_ context.Context, p db.DeliverClaimedIssueChannelNotificationParams) (pgtype.UUID, error) {
	f.delivered = p
	return f.row.IssueID, nil
}
func (f *fakeIssueChannelNotificationQueries) FailClaimedIssueChannelNotification(_ context.Context, p db.FailClaimedIssueChannelNotificationParams) (pgtype.UUID, error) {
	f.failed = p
	return f.row.IssueID, nil
}
func (f *fakeIssueChannelNotificationQueries) RetryClaimedIssueChannelNotification(_ context.Context, p db.RetryClaimedIssueChannelNotificationParams) (db.IssueChannelNotification, error) {
	f.retried = p
	return f.row, nil
}
func (f *fakeIssueChannelNotificationQueries) ReconcileClaimedIssueChannelNotification(_ context.Context, p db.ReconcileClaimedIssueChannelNotificationParams) (db.IssueChannelNotification, error) {
	f.reconcile = p
	return f.row, nil
}
func (f *fakeIssueChannelNotificationQueries) RetryReconcilingIssueChannelNotification(_ context.Context, p db.RetryReconcilingIssueChannelNotificationParams) (db.IssueChannelNotification, error) {
	f.retryReconcile = p
	return f.row, nil
}
func (f *fakeIssueChannelNotificationQueries) DeliverReconcilingIssueChannelNotification(_ context.Context, p db.DeliverReconcilingIssueChannelNotificationParams) (pgtype.UUID, error) {
	f.deliveredReconcile = p
	return f.row.IssueID, nil
}
func (f *fakeIssueChannelNotificationQueries) FailReconcilingIssueChannelNotification(_ context.Context, p db.FailReconcilingIssueChannelNotificationParams) (pgtype.UUID, error) {
	f.failedReconcile = p
	return f.row.IssueID, nil
}

type errorIssueChannelNotificationSender struct{ err error }

func (s errorIssueChannelNotificationSender) Send(context.Context, db.DBTX, db.IssueChannelNotification) (channelslack.IssueNotificationResult, error) {
	return channelslack.IssueNotificationResult{}, s.err
}
func (s errorIssueChannelNotificationSender) Reconcile(context.Context, db.DBTX, db.IssueChannelNotification) (channelslack.IssueNotificationResult, bool, error) {
	return channelslack.IssueNotificationResult{}, false, s.err
}

type reconcileIssueChannelNotificationSender struct {
	result channelslack.IssueNotificationResult
	found  bool
	err    error
	sends  int
}

func (s *reconcileIssueChannelNotificationSender) Send(context.Context, db.DBTX, db.IssueChannelNotification) (channelslack.IssueNotificationResult, error) {
	s.sends++
	return channelslack.IssueNotificationResult{}, errors.New("reconciling rows must not POST")
}

func (s *reconcileIssueChannelNotificationSender) Reconcile(context.Context, db.DBTX, db.IssueChannelNotification) (channelslack.IssueNotificationResult, bool, error) {
	return s.result, s.found, s.err
}

type deadlineIssueChannelNotificationSender struct {
	deadline time.Time
	ok       bool
}

type errorIssueChannelNotificationTxStarter struct{ err error }

func (s errorIssueChannelNotificationTxStarter) Begin(context.Context) (pgx.Tx, error) {
	return nil, s.err
}

func (s *deadlineIssueChannelNotificationSender) Send(ctx context.Context, _ db.DBTX, _ db.IssueChannelNotification) (channelslack.IssueNotificationResult, error) {
	s.deadline, s.ok = ctx.Deadline()
	return channelslack.IssueNotificationResult{}, context.DeadlineExceeded
}
func (s *deadlineIssueChannelNotificationSender) Reconcile(ctx context.Context, _ db.DBTX, _ db.IssueChannelNotification) (channelslack.IssueNotificationResult, bool, error) {
	s.deadline, s.ok = ctx.Deadline()
	return channelslack.IssueNotificationResult{}, false, context.DeadlineExceeded
}

type classifiedWorkerError struct {
	permanent bool
	retry     time.Duration
	deferSend bool
	ambiguous bool
}

func (e classifiedWorkerError) Error() string             { return "delivery failed" }
func (e classifiedWorkerError) Permanent() bool           { return e.permanent }
func (e classifiedWorkerError) RetryAfter() time.Duration { return e.retry }
func (e classifiedWorkerError) Defer() bool               { return e.deferSend }
func (e classifiedWorkerError) Ambiguous() bool           { return e.ambiguous }

func TestIssueChannelNotificationWorker_HonorsRetryAfter(t *testing.T) {
	queries := &fakeIssueChannelNotificationQueries{row: workerTestIssueChannelNotification()}
	started := time.Now()
	worker := NewIssueChannelNotificationWorker(queries, nil, errorIssueChannelNotificationSender{
		err: classifiedWorkerError{retry: 17 * time.Second},
	})
	worked, err := worker.ProcessNext(context.Background())
	if err != nil || !worked {
		t.Fatalf("ProcessNext = (%v, %v), want retry", worked, err)
	}
	if !queries.retried.AvailableAt.Valid || queries.retried.AvailableAt.Time.Before(started.Add(16*time.Second)) {
		t.Fatalf("retry available_at = %v, want Slack Retry-After", queries.retried.AvailableAt)
	}
	if queries.failed.IssueID.Valid || queries.delivered.IssueID.Valid {
		t.Fatal("retryable delivery reached a terminal state")
	}
}

func TestIssueChannelNotificationWorker_RefusesUnauditedDelivery(t *testing.T) {
	queries := &fakeIssueChannelNotificationQueries{
		row: workerTestIssueChannelNotification(), auditErr: errors.New("audit unavailable"),
	}
	sender := &reconcileIssueChannelNotificationSender{}
	worker := NewIssueChannelNotificationWorker(queries, nil, sender)

	worked, err := worker.ProcessNext(context.Background())
	if !worked || err == nil {
		t.Fatalf("ProcessNext = (%v, %v), want claimed audit failure", worked, err)
	}
	if sender.sends != 0 || queries.retried.IssueID.Valid || queries.failed.IssueID.Valid {
		t.Fatal("unaudited attempt reached transport or mutated delivery state")
	}
	if queries.row.Attempts != 0 {
		t.Fatalf("audit failure consumed %d attempts, want 0", queries.row.Attempts)
	}
}

func TestIssueChannelNotificationWorker_PreTransportFailureDoesNotConsumeDeliveryAttempt(t *testing.T) {
	queries := &fakeIssueChannelNotificationQueries{row: workerTestIssueChannelNotification()}
	worker := NewIssueChannelNotificationWorker(queries, errorIssueChannelNotificationTxStarter{
		err: errors.New("database unavailable"),
	}, errorIssueChannelNotificationSender{})

	worked, err := worker.ProcessNext(context.Background())
	if !worked || err == nil {
		t.Fatalf("ProcessNext = (%v, %v), want claimed pre-transport failure", worked, err)
	}
	if queries.row.Attempts != 0 || queries.audited.IssueID.Valid {
		t.Fatalf("pre-transport failure = attempts %d audit %#v, want neither", queries.row.Attempts, queries.audited)
	}
	if queries.row.ClaimCount != 1 {
		t.Fatalf("pre-transport claim count = %d, want durable replay marker 1", queries.row.ClaimCount)
	}
}

func TestIssueChannelNotificationWorker_BoundsSlackAttemptAndRetriesTimeout(t *testing.T) {
	queries := &fakeIssueChannelNotificationQueries{row: workerTestIssueChannelNotification()}
	sender := &deadlineIssueChannelNotificationSender{}
	started := time.Now()
	worker := NewIssueChannelNotificationWorker(queries, nil, sender)

	worked, err := worker.ProcessNext(context.Background())
	if err != nil || !worked {
		t.Fatalf("ProcessNext = (%v, %v), want retry after bounded timeout", worked, err)
	}
	if !sender.ok {
		t.Fatal("Slack sender context has no deadline")
	}
	if timeout := sender.deadline.Sub(started); timeout < 29*time.Second || timeout > 31*time.Second {
		t.Fatalf("Slack sender deadline = %v after start, want about %v", timeout, issueChannelNotificationSendTimeout)
	}
	if !queries.retried.IssueID.Valid {
		t.Fatal("deadline-exceeded delivery was not scheduled for retry")
	}
	if queries.failed.IssueID.Valid || queries.delivered.IssueID.Valid {
		t.Fatal("deadline-exceeded delivery reached a terminal state")
	}
}

func TestIssueChannelNotificationWorker_PermanentErrorDeadLetters(t *testing.T) {
	queries := &fakeIssueChannelNotificationQueries{row: workerTestIssueChannelNotification()}
	worker := NewIssueChannelNotificationWorker(queries, nil, errorIssueChannelNotificationSender{
		err: classifiedWorkerError{permanent: true},
	})
	worked, err := worker.ProcessNext(context.Background())
	if err != nil || !worked {
		t.Fatalf("ProcessNext = (%v, %v), want failed terminal row", worked, err)
	}
	if !queries.failed.IssueID.Valid || queries.retried.IssueID.Valid {
		t.Fatal("permanent error was not moved directly to failed")
	}
}

func TestIssueChannelNotificationWorker_FinalAmbiguousSendEntersReadOnlyReconciliation(t *testing.T) {
	row := workerTestIssueChannelNotification()
	row.Attempts = issueChannelNotificationMaxAttempts - 1
	queries := &fakeIssueChannelNotificationQueries{row: row}
	worker := NewIssueChannelNotificationWorker(queries, nil, errorIssueChannelNotificationSender{
		err: classifiedWorkerError{ambiguous: true},
	})

	worked, err := worker.ProcessNext(context.Background())
	if err != nil || !worked {
		t.Fatalf("ProcessNext = (%v, %v), want reconciliation handoff", worked, err)
	}
	if !queries.reconcile.IssueID.Valid {
		t.Fatal("final ambiguous POST did not enter reconciliation-only state")
	}
	if queries.failed.IssueID.Valid || queries.retried.IssueID.Valid {
		t.Fatal("final ambiguous POST was retried or dead-lettered before read-only reconciliation")
	}
}

func TestIssueChannelNotificationWorker_FinalDefiniteFailureDoesNotEnterReconciliation(t *testing.T) {
	row := workerTestIssueChannelNotification()
	row.Attempts = issueChannelNotificationMaxAttempts - 1
	queries := &fakeIssueChannelNotificationQueries{row: row}
	worker := NewIssueChannelNotificationWorker(queries, nil, errorIssueChannelNotificationSender{
		err: classifiedWorkerError{},
	})

	worked, err := worker.ProcessNext(context.Background())
	if err != nil || !worked {
		t.Fatalf("ProcessNext = (%v, %v), want terminal definite failure", worked, err)
	}
	if !queries.failed.IssueID.Valid || queries.reconcile.IssueID.Valid {
		t.Fatal("definite pre-POST failure entered ambiguous reconciliation")
	}
}

func TestIssueChannelNotificationWorker_ReconciliationFindsAmbiguousSuccessWithoutPosting(t *testing.T) {
	row := workerTestIssueChannelNotification()
	row.Status = "reconciling"
	sender := &reconcileIssueChannelNotificationSender{
		result: channelslack.IssueNotificationResult{MessageTS: "1700000000.9"},
		found:  true,
	}
	queries := &fakeIssueChannelNotificationQueries{row: row}
	worker := NewIssueChannelNotificationWorker(queries, nil, sender)

	worked, err := worker.ProcessNext(context.Background())
	if err != nil || !worked {
		t.Fatalf("ProcessNext = (%v, %v), want reconciled delivery", worked, err)
	}
	if sender.sends != 0 {
		t.Fatalf("reconciliation POST calls = %d, want 0", sender.sends)
	}
	if !queries.deliveredReconcile.IssueID.Valid || queries.deliveredReconcile.SlackMessageTs.String != "1700000000.9" {
		t.Fatal("metadata marker did not deliver the reconciling row")
	}
}

func TestIssueChannelNotificationWorker_ReconciliationConfirmsAbsenceAndFails(t *testing.T) {
	row := workerTestIssueChannelNotification()
	row.Status = "reconciling"
	queries := &fakeIssueChannelNotificationQueries{row: row}
	worker := NewIssueChannelNotificationWorker(queries, nil, &reconcileIssueChannelNotificationSender{})

	worked, err := worker.ProcessNext(context.Background())
	if err != nil || !worked {
		t.Fatalf("ProcessNext = (%v, %v), want definitive failed result", worked, err)
	}
	if !queries.failedReconcile.IssueID.Valid || queries.retryReconcile.IssueID.Valid {
		t.Fatal("definitive metadata absence did not fail the reconciling row")
	}
}

func TestIssueChannelNotificationWorker_ReconciliationQueryFailureRetriesWithoutPosting(t *testing.T) {
	row := workerTestIssueChannelNotification()
	row.Status = "reconciling"
	sender := &reconcileIssueChannelNotificationSender{err: errors.New("Slack read unavailable")}
	queries := &fakeIssueChannelNotificationQueries{row: row}
	worker := NewIssueChannelNotificationWorker(queries, nil, sender)

	worked, err := worker.ProcessNext(context.Background())
	if err != nil || !worked {
		t.Fatalf("ProcessNext = (%v, %v), want reconciliation retry", worked, err)
	}
	if sender.sends != 0 || !queries.retryReconcile.IssueID.Valid || queries.failedReconcile.IssueID.Valid {
		t.Fatal("transient reconciliation query failure did not schedule a read-only retry")
	}
}

func TestIssueChannelNotificationWorker_ReconciliationPermanentErrorDeadLetters(t *testing.T) {
	row := workerTestIssueChannelNotification()
	row.Status = "reconciling"
	queries := &fakeIssueChannelNotificationQueries{row: row}
	worker := NewIssueChannelNotificationWorker(queries, nil, &reconcileIssueChannelNotificationSender{
		err: classifiedWorkerError{permanent: true},
	})

	worked, err := worker.ProcessNext(context.Background())
	if err != nil || !worked {
		t.Fatalf("ProcessNext = (%v, %v), want permanent reconciliation failure", worked, err)
	}
	if !queries.failedReconcile.IssueID.Valid || queries.retryReconcile.IssueID.Valid {
		t.Fatal("permanent reconciliation error was not dead-lettered")
	}
}

func TestIssueChannelNotificationWorker_ReconciliationAttemptLimitDeadLetters(t *testing.T) {
	row := workerTestIssueChannelNotification()
	row.Status = "reconciling"
	row.ReconciliationAttempts = issueChannelNotificationMaxReconciliations - 1
	queries := &fakeIssueChannelNotificationQueries{row: row}
	worker := NewIssueChannelNotificationWorker(queries, nil, &reconcileIssueChannelNotificationSender{
		err: errors.New("Slack read unavailable"),
	})

	worked, err := worker.ProcessNext(context.Background())
	if err != nil || !worked {
		t.Fatalf("ProcessNext = (%v, %v), want reconciliation attempt-limit failure", worked, err)
	}
	if !queries.failedReconcile.IssueID.Valid || queries.retryReconcile.IssueID.Valid {
		t.Fatal("final reconciliation error did not dead-letter at the attempt limit")
	}
}

func TestIssueChannelNotificationWorker_CanceledContextLeavesLeaseForRecovery(t *testing.T) {
	queries := &fakeIssueChannelNotificationQueries{row: workerTestIssueChannelNotification()}
	worker := NewIssueChannelNotificationWorker(queries, nil, errorIssueChannelNotificationSender{
		err: context.Canceled,
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	worked, err := worker.ProcessNext(ctx)
	if !worked || !errors.Is(err, context.Canceled) {
		t.Fatalf("ProcessNext = (%v, %v), want claimed work with cancellation", worked, err)
	}
	if queries.retried.IssueID.Valid || queries.failed.IssueID.Valid || queries.delivered.IssueID.Valid {
		t.Fatal("shutdown cancellation mutated the claimed notification")
	}
}

func TestIssueChannelNotificationWorker_PostAuditReopenDefersRecordedAttempt(t *testing.T) {
	queries := &fakeIssueChannelNotificationQueries{row: workerTestIssueChannelNotification()}
	worker := NewIssueChannelNotificationWorker(queries, nil, errorIssueChannelNotificationSender{
		err: classifiedWorkerError{deferSend: true},
	})

	worked, err := worker.ProcessNext(context.Background())
	if err != nil || !worked {
		t.Fatalf("ProcessNext = (%v, %v), want deferred post-audit work", worked, err)
	}
	if !queries.deferred.IssueID.Valid {
		t.Fatal("post-audit reopen did not release the claim through defer")
	}
	if queries.retried.IssueID.Valid || queries.failed.IssueID.Valid || queries.delivered.IssueID.Valid {
		t.Fatal("post-audit reopen reached retry or terminal state")
	}
	if queries.row.Attempts != 1 {
		t.Fatalf("post-audit status defer attempts = %d, want recorded transport attempt 1", queries.row.Attempts)
	}
}

func TestIssueChannelNotificationWorker_LostTerminalOwnershipIsObservable(t *testing.T) {
	err := normalizeIssueChannelNotificationLeaseError("deliver", workerTestIssueChannelNotification(), pgx.ErrNoRows)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("lost terminal ownership error = %v, want pgx.ErrNoRows", err)
	}
}

func workerTestIssueChannelNotification() db.IssueChannelNotification {
	return db.IssueChannelNotification{
		IssueID:     pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
		WorkspaceID: pgtype.UUID{Bytes: [16]byte{3}, Valid: true},
		LeaseToken:  pgtype.UUID{Bytes: [16]byte{2}, Valid: true},
		Status:      "queued",
	}
}

var _ error = classifiedWorkerError{}
