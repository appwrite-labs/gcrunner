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
// (completed when empty), answers the rerun request with rerunStatus, lists
// the run's latest jobs from latest (name to attempts), and counts the reruns
// it received.
type github struct {
	runStatus   string
	rerunStatus int
	latest      map[string][]int
	reruns      int
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

// failedJob delivers the completed task for a failed job "build" of run 7
// that ran on gcrunner-7-42 and reports the status handed to Cloud Tasks.
func failedJob(t *testing.T, attempt int, gh *github, preemptedAt ...time.Time) int {
	t.Helper()
	t.Setenv("GCP_PROJECT", "p")
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))

	originalSecret, originalTransport, originalOptions, originalDelete := loadSecret, githubClient.Transport, computeOptions, deleteVM
	t.Cleanup(func() {
		loadSecret, githubClient.Transport, computeOptions, deleteVM = originalSecret, originalTransport, originalOptions, originalDelete
	})
	loadSecret = func(_ context.Context, name string) (string, error) {
		return map[string]string{"gcrunner-app-id": "1", "gcrunner-private-key": keyPEM}[name], nil
	}
	githubClient.Transport = gh
	computeOptions = []option.ClientOption{option.WithEndpoint(operations(t, preemptedAt...).URL), option.WithoutAuthentication()}
	deleteVM = func(context.Context, string) error { return nil }

	body, err := json.Marshal(WorkflowJobEvent{
		Action: "completed",
		WorkflowJob: WorkflowJob{
			ID: 42, Name: "build", RunID: 7, RunAttempt: attempt, RunnerName: "gcrunner-7-42", Labels: gcrunnerLabels,
			Conclusion: "failure", StartedAt: jobStart, CompletedAt: jobEnd,
		},
		Repository: Repository{FullName: "appwrite-labs/cloud", Name: "cloud", Owner: RepositoryOwner{Login: "appwrite-labs"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/task/completed", strings.NewReader(string(body)))
	request.Header.Set("X-CloudTasks-TaskName", "task-42")
	response := httptest.NewRecorder()
	HandleTask(response, request)
	return response.Code
}

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
		if code := failedJob(t, c.attempt, gh, c.preemptedAt...); code != http.StatusOK {
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
		if code := failedJob(t, 1, gh, jobStart.Add(5*time.Minute)); code != c.code {
			t.Errorf("%s: status %d, want %d", name, code, c.code)
		}
	}
}

// GitHub refuses to rerun a job while any job of its run is still going, so
// asking would only fail. A busy run is retried later without asking and
// without an error, and the rerun goes out once the run has completed.
func TestAPreemptedJobIsRerunOnlyOnceItsRunHasCompleted(t *testing.T) {
	for name, c := range map[string]struct {
		runStatus string
		code      int
		reruns    int
	}{
		"run queued":      {"queued", http.StatusServiceUnavailable, 0},
		"run in progress": {"in_progress", http.StatusServiceUnavailable, 0},
		"run completed":   {"completed", http.StatusOK, 1},
	} {
		gh := &github{runStatus: c.runStatus, rerunStatus: http.StatusCreated}
		if code := failedJob(t, 1, gh, jobStart.Add(5*time.Minute)); code != c.code {
			t.Errorf("%s: status %d, want %d", name, code, c.code)
		}
		if gh.reruns != c.reruns {
			t.Errorf("%s: %d reruns, want %d", name, gh.reruns, c.reruns)
		}
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
