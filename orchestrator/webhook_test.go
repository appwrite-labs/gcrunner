package function

import (
	"context"
	"errors"
	"testing"
)

// completedJob drives handleCompleted for a job of run 7 and reports which VM
// it deleted, with busyVMs standing in for the runners GitHub says are working.
func completedJob(t *testing.T, jobID int64, runnerName string, labels []string, busyVMs ...string) string {
	t.Helper()

	deleted := ""
	originalDelete, originalBusy := deleteVM, runnerIsBusy
	t.Cleanup(func() { deleteVM, runnerIsBusy = originalDelete, originalBusy })

	deleteVM = func(_ context.Context, name string) error {
		if deleted != "" {
			t.Fatalf("deleted two VMs: %s and %s", deleted, name)
		}
		deleted = name
		return nil
	}
	runnerIsBusy = func(_ context.Context, _, _, name string) (bool, error) {
		for _, busy := range busyVMs {
			if busy == name {
				return true, nil
			}
		}
		return false, nil
	}

	event := WorkflowJobEvent{
		Action:      "completed",
		WorkflowJob: WorkflowJob{ID: jobID, RunID: 7, RunnerName: runnerName, Labels: labels},
		Repository:  Repository{FullName: "appwrite-labs/cloud", Name: "cloud", Owner: RepositoryOwner{Login: "appwrite-labs"}},
	}
	if err := handleCompleted(context.Background(), event); err != nil {
		t.Fatalf("handleCompleted: %v", err)
	}
	return deleted
}

var gcrunnerLabels = []string{"gcrunner=7/machine=n2d-standard-4"}

func TestCompletedJobDeletesTheVMThatRanIt(t *testing.T) {
	if got := completedJob(t, 42, "gcrunner-7-42", gcrunnerLabels); got != "gcrunner-7-42" {
		t.Errorf("deleted %q, want gcrunner-7-42", got)
	}
}

// GitHub hands a queued job to any idle runner whose labels cover it, so the VM
// created for this job can be the one still running another job.
func TestCompletedJobLeavesTheVMThatIsRunningAnotherJob(t *testing.T) {
	if got := completedJob(t, 42, "gcrunner-7-41", gcrunnerLabels); got != "gcrunner-7-41" {
		t.Errorf("deleted %q, want the VM that ran the job, gcrunner-7-41", got)
	}
	if got := completedJob(t, 41, "", gcrunnerLabels, "gcrunner-7-41"); got != "" {
		t.Errorf("deleted %q, want nothing while that VM is busy", got)
	}
}

func TestCancelledJobDeletesItsIdleVM(t *testing.T) {
	if got := completedJob(t, 42, "", gcrunnerLabels); got != "gcrunner-7-42" {
		t.Errorf("deleted %q, want gcrunner-7-42", got)
	}
}

func TestCompletedJobOnAnotherProviderDeletesNothing(t *testing.T) {
	if got := completedJob(t, 42, "runs-on-i-0abc", []string{"runs-on=7/runner=build"}); got != "" {
		t.Errorf("deleted %q, want nothing for a job that is not ours", got)
	}
}

func TestCompletedJobKeepsTheVMWhenGitHubCannotBeReached(t *testing.T) {
	originalBusy := runnerIsBusy
	originalDelete := deleteVM
	t.Cleanup(func() { runnerIsBusy, deleteVM = originalBusy, originalDelete })

	runnerIsBusy = func(_ context.Context, _, _, _ string) (bool, error) {
		return false, errors.New("GitHub returned 502")
	}
	deleteVM = func(_ context.Context, name string) error {
		t.Fatalf("deleted %s without knowing whether it was busy", name)
		return nil
	}

	event := WorkflowJobEvent{
		WorkflowJob: WorkflowJob{ID: 42, RunID: 7, Labels: gcrunnerLabels},
		Repository:  Repository{Name: "cloud", Owner: RepositoryOwner{Login: "appwrite-labs"}},
	}
	if err := handleCompleted(context.Background(), event); err == nil {
		t.Error("handleCompleted returned nil, want the error so Cloud Tasks retries")
	}
}
