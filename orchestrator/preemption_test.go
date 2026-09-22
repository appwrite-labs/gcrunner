package function

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/option"
)

var (
	jobStart = time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	jobEnd   = jobStart.Add(20 * time.Minute)
)

// github stands in for api.github.com: it reports the run as runStatus
// (completed when empty), answers the rerun request with rerunStatus and the
// check run with checkStatus, lists the run's latest jobs from latest (name
// to attempts), and records what it was asked to do.
type github struct {
	runStatus   string
	rerunStatus int
	checkStatus int
	latest      map[string][]int
	reruns      int
	cancels     int
	checks      []map[string]any
}

func (g *github) RoundTrip(req *http.Request) (*http.Response, error) {
	answer := func(status int, body string) (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	}
	switch {
	case req.URL.Path == "/users/appwrite-labs/installation":
		return answer(http.StatusOK, `{"id": 5}`)
	case req.URL.Path == "/app/installations/5/access_tokens":
		return answer(http.StatusCreated, `{"token": "ghs_test"}`)
	case req.URL.Path == "/repos/appwrite-labs/cloud/actions/runs/7":
		status := g.runStatus
		if status == "" {
			status = "completed"
		}
		return answer(http.StatusOK, fmt.Sprintf(`{"id": 7, "status": %q}`, status))
	case req.URL.Path == "/repos/appwrite-labs/cloud/actions/jobs/42/rerun":
		g.reruns++
		return answer(g.rerunStatus, `{"message": "This workflow is already running"}`)
	case strings.HasSuffix(req.URL.Path, "/actions/runners/generate-jitconfig"):
		return answer(http.StatusCreated, `{"encoded_jit_config": "jit"}`)
	case strings.HasSuffix(req.URL.Path, "/actions/runners"):
		return answer(http.StatusOK, `{"runners": []}`)
	case req.URL.Path == "/repos/appwrite-labs/cloud/check-runs":
		var check map[string]any
		if err := json.NewDecoder(req.Body).Decode(&check); err != nil {
			return answer(http.StatusBadRequest, `{}`)
		}
		g.checks = append(g.checks, check)
		return answer(g.checkStatus, `{}`)
	case req.URL.Path == "/repos/appwrite-labs/cloud/actions/runs/7/cancel":
		g.cancels++
		return answer(http.StatusAccepted, ``)
	case req.URL.Path == "/repos/appwrite-labs/cloud/actions/runs/7/jobs" && req.URL.Query().Get("filter") == "latest":
		var jobs []string
		for name, attempts := range g.latest {
			for _, attempt := range attempts {
				jobs = append(jobs, fmt.Sprintf(`{"name": %q, "run_attempt": %d}`, name, attempt))
			}
		}
		return answer(http.StatusOK, fmt.Sprintf(`{"jobs": [%s]}`, strings.Join(jobs, ",")))
	}
	return answer(http.StatusNotFound, `{}`)
}

// operations stands in for the Compute operations API, listing preemptions of
// gcrunner-7-42 at the given times plus one of another VM inside the job
// window. It accepts only the filter the real API does.
func operations(t *testing.T, preemptedAt ...time.Time) *httptest.Server {
	t.Helper()
	operation := func(instance string, at time.Time) string {
		return fmt.Sprintf(`{"operationType": "compute.instances.preempted", "insertTime": %q, `+
			`"targetLink": "https://www.googleapis.com/compute/v1/projects/p/zones/us-central1-a/instances/%s"}`, at.Format(time.RFC3339Nano), instance)
	}
	operations := []string{operation("gcrunner-7-41", jobStart.Add(10*time.Minute))}
	for _, at := range preemptedAt {
		operations = append(operations, operation("gcrunner-7-42", at))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if filter := r.URL.Query().Get("filter"); filter != `operationType = "compute.instances.preempted"` {
			t.Errorf("filter %q is not one the Compute API accepts", filter)
			http.Error(w, `{"error": {"message": "Invalid list filter expression."}}`, http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, `{"items": {"zones/us-central1-a": {"operations": [%s]}}}`, strings.Join(operations, ","))
	}))
	t.Cleanup(server.Close)
	return server
}

// scheduled records the tasks the handler scheduled for later.
type scheduled struct {
	bodies [][]byte
	at     []time.Time
}

// useGitHub puts gh behind githubClient with App credentials that sign.
func useGitHub(t *testing.T, gh *github) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	originalSecret, originalTransport := loadSecret, githubClient.Transport
	t.Cleanup(func() { loadSecret, githubClient.Transport = originalSecret, originalTransport })
	loadSecret = func(_ context.Context, name string) (string, error) {
		return map[string]string{"gcrunner-app-id": "1", "gcrunner-private-key": keyPEM}[name], nil
	}
	githubClient.Transport = gh
}

// deliverTask posts event to the task path as Cloud Tasks does and reports
// the status handed back.
func deliverTask(t *testing.T, path string, event WorkflowJobEvent) int {
	t.Helper()
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	return deliverBody(t, path, body)
}

// deliverBody posts a task body as Cloud Tasks does, so a scheduled task's
// body can be delivered as is, and reports the status handed back.
func deliverBody(t *testing.T, path string, body []byte) int {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	request.Header.Set("X-CloudTasks-TaskName", "task-42")
	response := httptest.NewRecorder()
	HandleTask(response, request)
	return response.Code
}

// failedJob delivers the completed task for a failed job "build" of run 7
// that ran on gcrunner-7-42 and reports the status handed to Cloud Tasks
// along with the tasks it scheduled.
func failedJob(t *testing.T, attempt int, gh *github, preemptedAt ...time.Time) (int, *scheduled) {
	t.Helper()
	body, err := json.Marshal(WorkflowJobEvent{
		Action: "completed",
		WorkflowJob: WorkflowJob{
			ID: 42, Name: "build", RunID: 7, RunAttempt: attempt, RunnerName: "gcrunner-7-42", Labels: gcrunnerLabels,
			Conclusion: "failure", StartedAt: jobStart, CompletedAt: jobEnd,
		},
		Repository: cloudRepository,
	})
	if err != nil {
		t.Fatal(err)
	}
	return deliver(t, body, gh, preemptedAt...)
}

// deliver hands a completed task with the given body to the handler against
// the GitHub stand-in and the given preemptions of gcrunner-7-42.
func deliver(t *testing.T, body []byte, gh *github, preemptedAt ...time.Time) (int, *scheduled) {
	t.Helper()
	t.Setenv("GCP_PROJECT", "p")
	useGitHub(t, gh)
	originalOptions, originalDelete, originalSchedule := computeOptions, deleteVM, schedule
	t.Cleanup(func() { computeOptions, deleteVM, schedule = originalOptions, originalDelete, originalSchedule })
	computeOptions = []option.ClientOption{option.WithEndpoint(operations(t, preemptedAt...).URL), option.WithoutAuthentication()}
	deleteVM = func(context.Context, string) error { return nil }
	later := &scheduled{}
	schedule = func(_ context.Context, path string, body []byte, _ string, at time.Time) error {
		if path != "/task/completed" {
			t.Errorf("scheduled %s, want /task/completed", path)
		}
		later.bodies = append(later.bodies, body)
		later.at = append(later.at, at)
		return nil
	}
	return deliverBody(t, "/task/completed", body), later
}

var cloudRepository = Repository{FullName: "appwrite-labs/cloud", Name: "cloud", Owner: RepositoryOwner{Login: "appwrite-labs"}}

func TestOnlyAJobPreemptedWhileRunningIsRerun(t *testing.T) {
	during := jobStart.Add(5 * time.Minute)
	for name, c := range map[string]struct {
		attempt     int
		preemptedAt []time.Time
		reruns      int
	}{
		"preempted mid-run":               {1, []time.Time{during}, 1},
		"failed on its own":               {1, nil, 0},
		"preempted after it finished":     {1, []time.Time{jobEnd.Add(time.Minute)}, 0},
		"preempted on the second attempt": {2, []time.Time{during}, 1},
		"preempted on the third attempt":  {3, []time.Time{during}, 0},
	} {
		gh := &github{rerunStatus: http.StatusCreated}
		if code, _ := failedJob(t, c.attempt, gh, c.preemptedAt...); code != http.StatusOK {
			t.Errorf("%s: status %d, want 200", name, code)
		}
		if gh.reruns != c.reruns {
			t.Errorf("%s: %d reruns, want %d", name, gh.reruns, c.reruns)
		}
	}
}

// GitHub refuses a rerun while the run is busy, when an earlier delivery of
// the task already started it, and when the App lacks actions: write. Only a
// rerun that took is done, and a rerun gives the job a new id, so the proof
// is every job of its name being on a later attempt; anything less retries.
func TestARefusedRerunIsDoneOnlyOnceThisJobHasMovedOn(t *testing.T) {
	for name, c := range map[string]struct {
		latest map[string][]int
		code   int
	}{
		"already rerun":             {map[string][]int{"build": {2}, "lint": {1}}, http.StatusOK},
		"run busy or no permission": {map[string][]int{"build": {1}, "lint": {1}}, http.StatusInternalServerError},
		"only a sibling was rerun":  {map[string][]int{"build": {1}, "lint": {2}}, http.StatusInternalServerError},
		"a namesake was rerun":      {map[string][]int{"build": {2, 1}}, http.StatusInternalServerError},
	} {
		gh := &github{rerunStatus: http.StatusForbidden, latest: c.latest}
		if code, _ := failedJob(t, 1, gh, jobStart.Add(5*time.Minute)); code != c.code {
			t.Errorf("%s: status %d, want %d", name, code, c.code)
		}
	}
}

// GitHub refuses to rerun a job while any job of its run is still going, so
// asking would only fail. A busy run is looked at again from one task
// scheduled for later, not a retry, and that task sends the rerun once the
// run has completed.
func TestAPreemptedJobIsRerunOnlyOnceItsRunHasCompleted(t *testing.T) {
	during := jobStart.Add(5 * time.Minute)
	for _, status := range []string{"queued", "in_progress"} {
		gh := &github{runStatus: status, rerunStatus: http.StatusCreated}
		code, later := failedJob(t, 1, gh, during)
		if code != http.StatusOK {
			t.Errorf("run %s: status %d, want 200", status, code)
		}
		if gh.reruns != 0 {
			t.Errorf("run %s: %d reruns, want none yet", status, gh.reruns)
		}
		if len(later.bodies) != 1 {
			t.Fatalf("run %s: %d tasks scheduled, want 1", status, len(later.bodies))
		}
		if !later.at[0].After(time.Now()) {
			t.Errorf("run %s: task due at %s, want later", status, later.at[0])
		}

		gh.runStatus = "completed"
		code, again := deliver(t, later.bodies[0], gh, during)
		if code != http.StatusOK {
			t.Errorf("run %s then completed: status %d, want 200", status, code)
		}
		if gh.reruns != 1 || len(again.bodies) != 0 {
			t.Errorf("run %s then completed: %d reruns and %d tasks scheduled, want 1 and 0", status, gh.reruns, len(again.bodies))
		}
	}
}

// A rerun makes the run busy again, so a redelivery of the task after the
// rerun took must not mistake that for a run still to be waited on: the job
// already being on a later attempt settles it first.
func TestARerunThatAlreadyTookIsNotWaitedOn(t *testing.T) {
	gh := &github{runStatus: "in_progress", rerunStatus: http.StatusCreated, latest: map[string][]int{"build": {2}}}
	code, later := failedJob(t, 1, gh, jobStart.Add(5*time.Minute))
	if code != http.StatusOK {
		t.Errorf("status %d, want 200", code)
	}
	if gh.reruns != 0 || len(later.bodies) != 0 {
		t.Errorf("%d reruns and %d scheduled tasks, want none", gh.reruns, len(later.bodies))
	}
}

func TestOnlyFailuresThatRanHereCheckForPreemption(t *testing.T) {
	originalOptions := computeOptions
	t.Cleanup(func() { computeOptions = originalOptions })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("looked up Compute operations: %s", r.URL.Path)
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	computeOptions = []option.ClientOption{option.WithEndpoint(server.URL), option.WithoutAuthentication()}
	completedJob(t, 42, "gcrunner-7-42", gcrunnerLabels)
	completedJob(t, 42, "", gcrunnerLabels)
}
