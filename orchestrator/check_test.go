package function

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type pendingTask struct {
	path string
	body []byte
}

// recovery stands in for GitHub, Compute Engine and Cloud Tasks around job 42
// of run 7: GitHub reports the job as github.jobStatus, VM gcrunner-7-42 is in
// zone ("" when gone), and task names are unique as in Cloud Tasks.
type recovery struct {
	t             *testing.T
	github        *github
	zone          string
	runners       []RunnerLabels
	pending       []pendingTask
	names         map[string]bool
	failSchedule  int
	failProvision int
}

func newRecovery(t *testing.T, config string) *recovery {
	t.Helper()
	r := &recovery{t: t, github: &github{jobStatus: "queued"}, names: map[string]bool{}}
	useGitHub(t, r.github)
	originalProvision, originalZone, originalFetch, originalSchedule := provisionVM, findZone, fetchRepositoryFile, schedule
	t.Cleanup(func() {
		provisionVM, findZone, fetchRepositoryFile, schedule = originalProvision, originalZone, originalFetch, originalSchedule
		configCache.configs = map[string]configCacheEntry{}
	})
	r.useConfig(config)
	provisionVM = func(_ context.Context, _ WorkflowJobEvent, labels *RunnerLabels) error {
		if r.failProvision > 0 {
			r.failProvision--
			return errors.New("every zone out of quota")
		}
		r.runners = append(r.runners, *labels)
		return nil
	}
	findZone = func(_ context.Context, name string) (string, error) {
		if name != "gcrunner-7-42" {
			t.Errorf("looked up VM %s, want gcrunner-7-42", name)
		}
		return r.zone, nil
	}
	schedule = func(_ context.Context, path string, body []byte, name string, at time.Time) error {
		if r.failSchedule > 0 {
			r.failSchedule--
			return errors.New("cloud tasks unavailable")
		}
		if r.names[name] {
			return fmt.Errorf("create task: %w", status.Error(codes.AlreadyExists, "task exists"))
		}
		if !at.After(time.Now()) {
			t.Errorf("task %s due %s, want later", name, at)
		}
		r.names[name] = true
		r.pending = append(r.pending, pendingTask{path, body})
		return nil
	}
	return r
}

func (r *recovery) useConfig(config string) {
	configCache.configs = map[string]configCacheEntry{}
	fetchRepositoryFile = func(_ context.Context, _, _, _, _ string) ([]byte, error) { return []byte(config), nil }
}

func (r *recovery) deliver(task pendingTask) int {
	request := httptest.NewRequest(http.MethodPost, task.path, strings.NewReader(string(task.body)))
	request.Header.Set("X-CloudTasks-TaskName", "task-42")
	response := httptest.NewRecorder()
	HandleTask(response, request)
	return response.Code
}

// queue delivers the job's queued task, as the webhook does.
func (r *recovery) queue() {
	r.t.Helper()
	body := fmt.Sprintf(`{"action": "queued", "workflow_job": {"id": 42, "name": "deploy", "run_id": 7, "labels": ["gcrunner=7/runner=deploy"]}, ` +
		`"repository": {"full_name": "appwrite-labs/cloud", "name": "cloud", "owner": {"login": "appwrite-labs"}}}`)
	if code := r.deliver(pendingTask{"/task/queued", []byte(body)}); code != http.StatusOK {
		r.t.Fatalf("queued task: status %d, want 200", code)
	}
}

// next delivers the earliest scheduled task and reports its status, or false
// when none is left.
func (r *recovery) next() (int, bool) {
	if len(r.pending) == 0 {
		return 0, false
	}
	task := r.pending[0]
	r.pending = r.pending[1:]
	code := r.deliver(task)
	if code != http.StatusOK {
		r.pending = append([]pendingTask{task}, r.pending...)
	}
	return code, true
}

// drain delivers scheduled tasks, retrying failures as Cloud Tasks does,
// until none is left.
func (r *recovery) drain() {
	r.t.Helper()
	for range 20 {
		if _, ok := r.next(); !ok {
			return
		}
	}
	r.t.Fatal("checks never stopped")
}

const deployConfig = "runners:\n  deploy:\n    machine: n2d-standard-2\n"

func TestAJobStillQueuedAfterItsVMIsGoneGetsAtMostThreeNewRunners(t *testing.T) {
	r := newRecovery(t, deployConfig)
	r.queue()
	r.drain()
	if len(r.runners) != 4 {
		t.Errorf("%d runners, want the first and 3 replacements", len(r.runners))
	}
}

func TestACheckLeavesAJobThatDoesNotNeedANewRunner(t *testing.T) {
	for name, c := range map[string]struct {
		status, zone string
		checking     bool
	}{
		"VM booting or listening":                {"queued", "us-east1-b", true},
		"job started":                            {"in_progress", "", false},
		"job finished":                           {"completed", "", false},
		"job waiting on concurrency or approval": {"waiting", "", false},
	} {
		r := newRecovery(t, deployConfig)
		r.queue()
		r.github.jobStatus, r.zone = c.status, c.zone
		if code, _ := r.next(); code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200", name, code)
		}
		if len(r.runners) != 1 || (len(r.pending) == 1) != c.checking {
			t.Errorf("%s: %d runners and %d checks pending, want 1 runner and checking %t", name, len(r.runners), len(r.pending), c.checking)
		}
	}
}

func TestARetriedCheckKeepsCountOfTheRunnersItReplaced(t *testing.T) {
	for name, fail := range map[string]func(*recovery){
		"scheduling the next check failed": func(r *recovery) { r.failSchedule = 1 },
		"creating the VM failed":           func(r *recovery) { r.failProvision = 1 },
	} {
		r := newRecovery(t, deployConfig)
		r.queue()
		fail(r)
		if code, _ := r.next(); code != http.StatusInternalServerError {
			t.Fatalf("%s: status %d, want 500 so Cloud Tasks retries", name, code)
		}
		r.drain()
		if len(r.runners) != 4 {
			t.Errorf("%s: %d runners, want the first and 3 replacements", name, len(r.runners))
		}
	}
}

func TestAReplacementRunnerIgnoresConfigChangesSinceTheJobWasQueued(t *testing.T) {
	r := newRecovery(t, deployConfig)
	r.queue()
	r.useConfig("runners:\n  build:\n    machine: n2d-standard-8\n")
	if code, _ := r.next(); code != http.StatusOK {
		t.Fatalf("status %d, want 200", code)
	}
	if len(r.runners) != 2 || r.runners[1] != r.runners[0] || r.github.cancels != 0 {
		t.Errorf("runners %+v and %d cancels, want the queued runner twice and no cancel", r.runners, r.github.cancels)
	}
}
