package function

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// check delivers a task for job 42 of run 7 to path with GitHub reporting the
// job as status and VM gcrunner-7-42 in zone ("" when gone), and reports the
// VMs provisioned and the tasks scheduled.
func check(t *testing.T, path, status, zone string) (int, []scheduledTask) {
	t.Helper()
	useGitHub(t, &github{jobStatus: status})
	tasks := recordSchedule(t)
	originalProvision, originalZone, originalFetch := provisionVM, findZone, fetchRepositoryFile
	t.Cleanup(func() {
		provisionVM, findZone, fetchRepositoryFile = originalProvision, originalZone, originalFetch
		configCache.configs = map[string]configCacheEntry{}
	})
	configCache.configs = map[string]configCacheEntry{}
	fetchRepositoryFile = func(_ context.Context, _, _, _, _ string) ([]byte, error) { return nil, errConfigNotFound }
	provisioned := 0
	provisionVM = func(context.Context, WorkflowJobEvent, *RunnerLabels) error {
		provisioned++
		return nil
	}
	findZone = func(_ context.Context, name string) (string, error) {
		if name != "gcrunner-7-42" {
			t.Errorf("looked up VM %s, want gcrunner-7-42", name)
		}
		return zone, nil
	}

	event := WorkflowJobEvent{
		Action:      "queued",
		WorkflowJob: WorkflowJob{ID: 42, Name: "deploy", RunID: 7, Labels: gcrunnerLabels},
		Repository:  cloudRepository,
	}
	if code := deliverTask(t, path, event); code != http.StatusOK {
		t.Fatalf("%s: status %d, want 200", path, code)
	}
	return provisioned, *tasks
}

func TestAQueuedJobIsCheckedAfterItsVMIsCreated(t *testing.T) {
	provisioned, tasks := check(t, "/task/queued", "", "")
	if provisioned != 1 || len(tasks) != 1 {
		t.Fatalf("%d VMs and %d tasks, want 1 and 1", provisioned, len(tasks))
	}
	if tasks[0].path != "/task/check?provisioned=0" || !tasks[0].at.After(time.Now()) {
		t.Errorf("scheduled %s at %s, want /task/check?provisioned=0 later", tasks[0].path, tasks[0].at)
	}
}

func TestACheckReplacesOnlyTheRunnerOfAQueuedJobWhoseVMIsGone(t *testing.T) {
	for name, c := range map[string]struct {
		path, status, zone string
		provisioned        int
		next               string
	}{
		"runner dropped before the job reached it": {"/task/check?provisioned=0", "queued", "", 1, "/task/check?provisioned=1"},
		"VM still booting or listening":            {"/task/check?provisioned=1", "queued", "us-east1-b", 0, "/task/check?provisioned=1"},
		"replacements used up":                     {"/task/check?provisioned=3", "queued", "", 0, ""},
		"job started":                              {"/task/check?provisioned=0", "in_progress", "", 0, ""},
		"job finished":                             {"/task/check?provisioned=0", "completed", "", 0, ""},
		"job waiting on concurrency or approval":   {"/task/check?provisioned=0", "waiting", "", 0, ""},
	} {
		provisioned, tasks := check(t, c.path, c.status, c.zone)
		var next []string
		for _, task := range tasks {
			next = append(next, task.path)
		}
		if provisioned != c.provisioned || strings.Join(next, ",") != c.next {
			t.Errorf("%s: %d VMs and next check %q, want %d and %q", name, provisioned, next, c.provisioned, c.next)
		}
	}
}
