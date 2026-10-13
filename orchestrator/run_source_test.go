package function

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain points runSource at the job's own repository, so tests that are
// not about where a run comes from need no GitHub API for it.
func TestMain(m *testing.M) {
	runSource = func(_ context.Context, event WorkflowJobEvent) (string, error) {
		return event.Repository.FullName, nil
	}
	os.Exit(m.Run())
}

// withRunSourceLookup makes jobs look their run up through GitHub, starting
// from an empty cache.
func withRunSourceLookup(t *testing.T) {
	t.Helper()
	original := runSource
	runSource = lookupRunSource
	runSources.sources = map[string]runSourceEntry{}
	t.Cleanup(func() {
		runSource = original
		runSources.sources = map[string]runSourceEntry{}
	})
}

// A pull request from a fork runs the fork's copy of the workflow, which can
// ask for any gcrunner label, so its job gets a failed check and a cancelled
// run instead of a VM, even with a config and zones that would provision it.
// So does a run whose fork has been deleted since.
func TestARunFromAnotherRepositoryGetsNoVM(t *testing.T) {
	config := "runners:\n  deploy:\n    family: c3d\n    cpu: 4\n"
	gce := engine{zones: []string{"europe-west3-a"}, catalogue: map[string][]*MachineTypeInfo{"europe-west3-a": fourVCPUs}}
	for name, source := range map[string]string{"fork": "mallory/cloud", "deleted fork": ""} {
		withRunSourceLookup(t)
		gh := &github{checkStatus: http.StatusCreated, headRepository: source}
		if code := queuedJob(t, gh, config, gce); code != http.StatusOK {
			t.Errorf("%s: status %d, want 200 so Cloud Tasks does not retry", name, code)
		}
		if len(gh.checks) != 1 || gh.cancels != 1 {
			t.Fatalf("%s: %d check runs and %d cancels, want one of each", name, len(gh.checks), gh.cancels)
		}
		output, _ := gh.checks[0]["output"].(map[string]any)
		if summary, _ := output["summary"].(string); !strings.Contains(summary, "only runs jobs from appwrite-labs/cloud itself") {
			t.Errorf("%s: check run summary %q does not say why", name, summary)
		}
	}
}

func TestARunFromTheRepositoryItselfIsAllowed(t *testing.T) {
	withRunSourceLookup(t)
	useGitHub(t, &github{headRepository: "Appwrite-Labs/Cloud"})
	event := WorkflowJobEvent{WorkflowJob: WorkflowJob{ID: 42, RunID: 7}, Repository: cloudRepository}
	if err := checkRunSource(context.Background(), event); err != nil {
		t.Errorf("run from the repository itself refused: %v", err)
	}
}

// Not knowing where a run comes from is no reason to fail it: the task is
// retried.
func TestAFailedRunLookupIsRetried(t *testing.T) {
	withRunSourceLookup(t)
	gh := &github{checkStatus: http.StatusCreated, runLookupStatus: http.StatusBadGateway}
	if code := queuedJob(t, gh, "runners: {}\n", engine{}); code != http.StatusInternalServerError {
		t.Errorf("status %d, want 500 so Cloud Tasks retries", code)
	}
	if gh.cancels != 0 || len(gh.checks) != 0 {
		t.Errorf("failed the run (%d checks, %d cancels) without knowing where it comes from", len(gh.checks), gh.cancels)
	}
}

// The jobs of one run share a lookup, until it expires.
func TestJobsOfOneRunLookItUpOnce(t *testing.T) {
	withRunSourceLookup(t)
	gh := &github{headRepository: "appwrite-labs/cloud"}
	useGitHub(t, gh)
	now := jobStart
	runSources.nowFunc = func() time.Time { return now }
	t.Cleanup(func() { runSources.nowFunc = time.Now })
	for id := int64(1); id <= 3; id++ {
		event := WorkflowJobEvent{WorkflowJob: WorkflowJob{ID: id, RunID: 7}, Repository: cloudRepository}
		if err := checkRunSource(context.Background(), event); err != nil {
			t.Fatalf("job %d refused: %v", id, err)
		}
	}
	if gh.runLookups != 1 {
		t.Errorf("%d lookups for three jobs of one run, want 1", gh.runLookups)
	}
	now = now.Add(runSources.ttl)
	if err := checkRunSource(context.Background(), WorkflowJobEvent{WorkflowJob: WorkflowJob{ID: 4, RunID: 7}, Repository: cloudRepository}); err != nil {
		t.Fatal(err)
	}
	if gh.runLookups != 2 {
		t.Errorf("%d lookups, want the expired entry looked up again", gh.runLookups)
	}
}

// A failed lookup is not remembered: the retry asks GitHub again.
func TestAFailedRunLookupIsNotRemembered(t *testing.T) {
	withRunSourceLookup(t)
	gh := &github{headRepository: "appwrite-labs/cloud", runLookupStatus: http.StatusBadGateway}
	useGitHub(t, gh)
	event := WorkflowJobEvent{WorkflowJob: WorkflowJob{ID: 42, RunID: 7}, Repository: cloudRepository}
	if err := checkRunSource(context.Background(), event); err == nil {
		t.Fatal("lookup failure not reported")
	}
	gh.runLookupStatus = 0
	if err := checkRunSource(context.Background(), event); err != nil {
		t.Errorf("retry after GitHub recovered: %v", err)
	}
}
