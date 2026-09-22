package function

import (
	"context"
	"errors"
	"testing"
	"time"
)

var (
	jobStart = time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	jobEnd   = jobStart.Add(20 * time.Minute)
)

// failedJob drives handleCompleted for a failed job of run 7 that ran on
// gcrunner-7-42 and reports whether it was rerun. preemptedAt is when Compute
// Engine preempted the VM, or zero when it never did.
func failedJob(t *testing.T, attempt int, preemptedAt time.Time) bool {
	t.Helper()

	rerun := false
	originalDelete, originalBusy, originalPreempted, originalRerun := deleteVM, runnerIsBusy, vmPreempted, rerunJob
	t.Cleanup(func() {
		deleteVM, runnerIsBusy, vmPreempted, rerunJob = originalDelete, originalBusy, originalPreempted, originalRerun
	})
	deleteVM = func(context.Context, string) error { return nil }
	runnerIsBusy = func(context.Context, string, string, string) (bool, error) {
		t.Fatal("asked GitHub about the runner for a job that ran here")
		return false, nil
	}
	vmPreempted = func(_ context.Context, name string, from, until time.Time) (bool, error) {
		if name != "gcrunner-7-42" {
			t.Errorf("checked preemption of %q, want gcrunner-7-42", name)
		}
		return !preemptedAt.IsZero() && !preemptedAt.Before(from) && !preemptedAt.After(until), nil
	}
	rerunJob = func(_ context.Context, owner, repo string, jobID int64) error {
		if owner != "appwrite-labs" || repo != "cloud" || jobID != 42 {
			t.Errorf("rerun %s/%s job %d, want appwrite-labs/cloud job 42", owner, repo, jobID)
		}
		rerun = true
		return nil
	}

	event := WorkflowJobEvent{
		Action: "completed",
		WorkflowJob: WorkflowJob{
			ID: 42, RunID: 7, RunAttempt: attempt, RunnerName: "gcrunner-7-42", Labels: gcrunnerLabels,
			Conclusion: conclusionFailure, StartedAt: jobStart, CompletedAt: jobEnd,
		},
		Repository: Repository{FullName: "appwrite-labs/cloud", Name: "cloud", Owner: RepositoryOwner{Login: "appwrite-labs"}},
	}
	if err := handleCompleted(context.Background(), event); err != nil {
		t.Fatalf("handleCompleted: %v", err)
	}
	return rerun
}

func TestAJobPreemptedWhileRunningIsRerun(t *testing.T) {
	if !failedJob(t, 1, jobStart.Add(5*time.Minute)) {
		t.Error("preempted job was not rerun")
	}
}

// Real test failures never produce a preempted operation on the VM.
func TestAJobThatFailedOnItsOwnIsNotRerun(t *testing.T) {
	if failedJob(t, 1, time.Time{}) {
		t.Error("rerun a job that failed without a preemption")
	}
}

// A VM preempted after its job had finished did not cause that failure.
func TestAPreemptionOutsideTheJobWindowIsNotARerun(t *testing.T) {
	if failedJob(t, 1, jobEnd.Add(time.Minute)) {
		t.Error("rerun a job whose VM was preempted after it completed")
	}
}

func TestReruningStopsAtTheAttemptCap(t *testing.T) {
	if !failedJob(t, rerunAttempts-1, jobStart.Add(5*time.Minute)) {
		t.Errorf("attempt %d was not rerun", rerunAttempts-1)
	}
	if failedJob(t, rerunAttempts, jobStart.Add(5*time.Minute)) {
		t.Errorf("attempt %d was rerun past the cap", rerunAttempts)
	}
}

// A successful job never needs the operations lookup, and a failed job that
// never reached a runner has no VM preemption to look for.
func TestOnlyFailuresThatRanHereCheckForPreemption(t *testing.T) {
	originalPreempted := vmPreempted
	t.Cleanup(func() { vmPreempted = originalPreempted })
	vmPreempted = func(_ context.Context, name string, _, _ time.Time) (bool, error) {
		t.Errorf("checked preemption of %s", name)
		return false, nil
	}
	completedJob(t, 42, "gcrunner-7-42", gcrunnerLabels)
	completedJob(t, 42, "", gcrunnerLabels)
}

// The VM stays until the lookup succeeds, so Cloud Tasks retries the task
// and a preempted job is not quietly left failed.
func TestAFailedPreemptionLookupRetriesTheTask(t *testing.T) {
	originalDelete, originalPreempted := deleteVM, vmPreempted
	t.Cleanup(func() { deleteVM, vmPreempted = originalDelete, originalPreempted })
	deleteVM = func(context.Context, string) error {
		t.Error("deleted the VM before knowing whether it was preempted")
		return nil
	}
	vmPreempted = func(context.Context, string, time.Time, time.Time) (bool, error) {
		return false, errors.New("compute unavailable")
	}
	event := WorkflowJobEvent{
		WorkflowJob: WorkflowJob{ID: 42, RunID: 7, RunAttempt: 1, RunnerName: "gcrunner-7-42", Labels: gcrunnerLabels, Conclusion: conclusionFailure},
		Repository:  Repository{Name: "cloud", Owner: RepositoryOwner{Login: "appwrite-labs"}},
	}
	if err := handleCompleted(context.Background(), event); err == nil {
		t.Error("handleCompleted succeeded without knowing whether the VM was preempted")
	}
}
