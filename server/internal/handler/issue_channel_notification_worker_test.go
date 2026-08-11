package handler

import (
	"context"
	"errors"
	"testing"
	"time"

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
	return f.row, nil
}
func (f *fakeIssueChannelNotificationQueries) AuditClaimedIssueChannelNotificationAttempt(_ context.Context, p db.AuditClaimedIssueChannelNotificationAttemptParams) (pgtype.UUID, error) {
	f.audited = p
	return pgtype.UUID{Bytes: [16]byte{9}, Valid: true}, f.auditErr
}
func (f *fakeIssueChannelNotificationQueries) DeferClaimedIssueChannelNotification(_ context.Context, p db.DeferClaimedIssueChannelNotificationParams) (db.IssueChannelNotification, error) {
	f.deferred = p
	return f.row, nil
}
func (f *fakeIssueChannelNotificationQueries) DeliverClaimedIssueChannelNotification(_ context.Context, p db.DeliverClaimedIssueChannelNotificationParams) (db.IssueChannelNotification, error) {
	f.delivered = p
	return f.row, nil
}
func (f *fakeIssueChannelNotificationQueries) FailClaimedIssueChannelNotification(_ context.Context, p db.FailClaimedIssueChannelNotificationParams) (db.IssueChannelNotification, error) {
	f.failed = p
	return f.row, nil
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
func (f *fakeIssueChannelNotificationQueries) DeliverReconcilingIssueChannelNotification(_ context.Context, p db.DeliverReconcilingIssueChannelNotificationParams) (db.IssueChannelNotification, error) {
	f.deliveredReconcile = p
	return f.row, nil
}
func (f *fakeIssueChannelNotificationQueries) FailReconcilingIssueChannelNotification(_ context.Context, p db.FailReconcilingIssueChannelNotificationParams) (db.IssueChannelNotification, error) {
	f.failedReconcile = p
	return f.row, nil
}

type errorIssueChannelNotificationSender struct{ err error }

func (s errorIssueChannelNotificationSender) Send(context.Context, db.IssueChannelNotification) (channelslack.IssueNotificationResult, error) {
	return channelslack.IssueNotificationResult{}, s.err
}
func (s errorIssueChannelNotificationSender) Reconcile(context.Context, db.IssueChannelNotification) (channelslack.IssueNotificationResult, bool, error) {
	return channelslack.IssueNotificationResult{}, false, s.err
}

type reconcileIssueChannelNotificationSender struct {
	result channelslack.IssueNotificationResult
	found  bool
	err    error
	sends  int
}

func (s *reconcileIssueChannelNotificationSender) Send(context.Context, db.IssueChannelNotification) (channelslack.IssueNotificationResult, error) {
	s.sends++
	return channelslack.IssueNotificationResult{}, errors.New("reconciling rows must not POST")
}

func (s *reconcileIssueChannelNotificationSender) Reconcile(context.Context, db.IssueChannelNotification) (channelslack.IssueNotificationResult, bool, error) {
	return s.result, s.found, s.err
}

type deadlineIssueChannelNotificationSender struct {
	deadline time.Time
	ok       bool
}

func (s *deadlineIssueChannelNotificationSender) Send(ctx context.Context, _ db.IssueChannelNotification) (channelslack.IssueNotificationResult, error) {
	s.deadline, s.ok = ctx.Deadline()
	return channelslack.IssueNotificationResult{}, context.DeadlineExceeded
}
func (s *deadlineIssueChannelNotificationSender) Reconcile(ctx context.Context, _ db.IssueChannelNotification) (channelslack.IssueNotificationResult, bool, error) {
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
	worker := NewIssueChannelNotificationWorker(queries, errorIssueChannelNotificationSender{
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
	worker := NewIssueChannelNotificationWorker(queries, sender)

	worked, err := worker.ProcessNext(context.Background())
	if !worked || err == nil {
		t.Fatalf("ProcessNext = (%v, %v), want claimed audit failure", worked, err)
	}
	if sender.sends != 0 || queries.retried.IssueID.Valid || queries.failed.IssueID.Valid {
		t.Fatal("unaudited attempt reached transport or mutated delivery state")
	}
}

func TestIssueChannelNotificationWorker_BoundsSlackAttemptAndRetriesTimeout(t *testing.T) {
	queries := &fakeIssueChannelNotificationQueries{row: workerTestIssueChannelNotification()}
	sender := &deadlineIssueChannelNotificationSender{}
	started := time.Now()
	worker := NewIssueChannelNotificationWorker(queries, sender)

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
	worker := NewIssueChannelNotificationWorker(queries, errorIssueChannelNotificationSender{
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
	worker := NewIssueChannelNotificationWorker(queries, errorIssueChannelNotificationSender{
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
	worker := NewIssueChannelNotificationWorker(queries, errorIssueChannelNotificationSender{
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
	worker := NewIssueChannelNotificationWorker(queries, sender)

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
	worker := NewIssueChannelNotificationWorker(queries, &reconcileIssueChannelNotificationSender{})

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
	worker := NewIssueChannelNotificationWorker(queries, sender)

	worked, err := worker.ProcessNext(context.Background())
	if err != nil || !worked {
		t.Fatalf("ProcessNext = (%v, %v), want reconciliation retry", worked, err)
	}
	if sender.sends != 0 || !queries.retryReconcile.IssueID.Valid || queries.failedReconcile.IssueID.Valid {
		t.Fatal("transient reconciliation query failure did not schedule a read-only retry")
	}
}

func TestIssueChannelNotificationWorker_CanceledContextLeavesLeaseForRecovery(t *testing.T) {
	queries := &fakeIssueChannelNotificationQueries{row: workerTestIssueChannelNotification()}
	worker := NewIssueChannelNotificationWorker(queries, errorIssueChannelNotificationSender{
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

func TestIssueChannelNotificationWorker_ReopenRaceDefersWithoutAttempt(t *testing.T) {
	queries := &fakeIssueChannelNotificationQueries{row: workerTestIssueChannelNotification()}
	worker := NewIssueChannelNotificationWorker(queries, errorIssueChannelNotificationSender{
		err: classifiedWorkerError{deferSend: true},
	})

	worked, err := worker.ProcessNext(context.Background())
	if err != nil || !worked {
		t.Fatalf("ProcessNext = (%v, %v), want deferred claimed work", worked, err)
	}
	if !queries.deferred.IssueID.Valid {
		t.Fatal("reopen race did not release the claim through defer")
	}
	if queries.retried.IssueID.Valid || queries.failed.IssueID.Valid || queries.delivered.IssueID.Valid {
		t.Fatal("reopen race consumed an attempt or reached a terminal state")
	}
}

func workerTestIssueChannelNotification() db.IssueChannelNotification {
	return db.IssueChannelNotification{
		IssueID:    pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
		LeaseToken: pgtype.UUID{Bytes: [16]byte{2}, Valid: true},
		Status:     "queued",
	}
}

var _ error = classifiedWorkerError{}
