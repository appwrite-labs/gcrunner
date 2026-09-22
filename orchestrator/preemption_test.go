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
	"sync"
	"testing"
	"time"

	"google.golang.org/api/option"
)

var (
	jobStart = time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	jobEnd   = jobStart.Add(20 * time.Minute)
)

// github stands in for api.github.com behind githubClient. It issues
// installation tokens, answers the rerun request with rerunStatus, reports
// the run as being on runAttempt, and records every rerun it was asked for.
type github struct {
	t           *testing.T
	rerunStatus int
	runAttempt  int
	mu          sync.Mutex
	reruns      []string
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
	case strings.HasSuffix(req.URL.Path, "/rerun") && req.Method == http.MethodPost:
		if req.Header.Get("Authorization") != "Bearer ghs_test" {
			g.t.Errorf("rerun sent without the installation token")
		}
		g.mu.Lock()
		g.reruns = append(g.reruns, req.URL.Path)
		g.mu.Unlock()
		if g.rerunStatus == http.StatusCreated {
			return answer(http.StatusCreated, `{}`)
		}
		return answer(g.rerunStatus, `{"message": "This workflow is already running"}`)
	case strings.HasPrefix(req.URL.Path, "/repos/appwrite-labs/cloud/actions/runs/"):
		return answer(http.StatusOK, fmt.Sprintf(`{"run_attempt": %d}`, g.runAttempt))
	}
	g.t.Errorf("unexpected GitHub request %s %s", req.Method, req.URL.Path)
	return answer(http.StatusNotFound, `{}`)
}

// operations stands in for the Compute Engine operations API: a project whose
// only recorded preemptions are those of gcrunner-7-42 at the given times.
func operations(t *testing.T, preemptedAt ...time.Time) *httptest.Server {
	t.Helper()
	var operations []string
	for _, at := range preemptedAt {
		operations = append(operations, fmt.Sprintf(`{"operationType": "compute.instances.preempted", "status": "DONE", `+
			`"targetLink": "https://www.googleapis.com/compute/v1/projects/p/zones/us-central1-a/instances/gcrunner-7-42", `+
			`"insertTime": %q}`, at.Format(time.RFC3339Nano)))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/projects/p/aggregated/operations") {
			t.Errorf("unexpected Compute request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"items": {"zones/us-central1-a": {"operations": [%s]}}}`, strings.Join(operations, ","))
	}))
	t.Cleanup(server.Close)
	return server
}

// failedJob delivers the completed task for a failed job of run 7 that ran
// on gcrunner-7-42 and reports the HTTP status handed back to Cloud Tasks
// and the reruns GitHub received.
func failedJob(t *testing.T, attempt int, gh *github, preemptedAt ...time.Time) (int, []string) {
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
	gh.t = t
	githubClient.Transport = gh
	computeOptions = []option.ClientOption{option.WithEndpoint(operations(t, preemptedAt...).URL), option.WithoutAuthentication()}
	deleteVM = func(context.Context, string) error { return nil }

	body, err := json.Marshal(WorkflowJobEvent{
		Action: "completed",
		WorkflowJob: WorkflowJob{
			ID: 42, RunID: 7, RunAttempt: attempt, RunnerName: "gcrunner-7-42", Labels: gcrunnerLabels,
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
	return response.Code, gh.reruns
}

const rerunPath = "/repos/appwrite-labs/cloud/actions/jobs/42/rerun"

func TestAJobPreemptedWhileRunningIsRerun(t *testing.T) {
	code, reruns := failedJob(t, 1, &github{rerunStatus: http.StatusCreated}, jobStart.Add(5*time.Minute))
	if code != http.StatusOK {
		t.Errorf("status %d, want 200", code)
	}
	if len(reruns) != 1 || reruns[0] != rerunPath {
		t.Errorf("reruns %v, want one of %s", reruns, rerunPath)
	}
}

// Real test failures never produce a preempted operation on the VM.
func TestAJobThatFailedOnItsOwnIsNotRerun(t *testing.T) {
	if _, reruns := failedJob(t, 1, &github{}); len(reruns) != 0 {
		t.Errorf("rerun a job that failed without a preemption: %v", reruns)
	}
}

// A VM preempted after its job had finished did not cause that failure.
func TestAPreemptionOutsideTheJobWindowIsNotARerun(t *testing.T) {
	if _, reruns := failedJob(t, 1, &github{}, jobEnd.Add(time.Minute)); len(reruns) != 0 {
		t.Errorf("rerun a job whose VM was preempted after it completed: %v", reruns)
	}
}

// Two automatic reruns are enough: a job preempted three times in a row is
// telling us something else is wrong.
func TestRerunsStopAtTheThirdAttempt(t *testing.T) {
	if _, reruns := failedJob(t, 2, &github{rerunStatus: http.StatusCreated}, jobStart.Add(5*time.Minute)); len(reruns) != 1 {
		t.Errorf("second attempt: reruns %v, want one", reruns)
	}
	if _, reruns := failedJob(t, 3, &github{rerunStatus: http.StatusCreated}, jobStart.Add(5*time.Minute)); len(reruns) != 0 {
		t.Errorf("third attempt: reruns %v, want none", reruns)
	}
}

// Cloud Tasks may deliver the completed task twice. GitHub refuses the second
// rerun, and the run already being on the next attempt shows the first one
// took, so the task is acknowledged rather than retried forever.
func TestARerunGitHubAlreadyStartedIsDone(t *testing.T) {
	code, _ := failedJob(t, 1, &github{rerunStatus: http.StatusForbidden, runAttempt: 2}, jobStart.Add(5*time.Minute))
	if code != http.StatusOK {
		t.Errorf("status %d, want 200 so Cloud Tasks drops the task", code)
	}
}

// GitHub refuses a rerun while the run's other jobs are still going, and
// refuses it for good when the App lacks actions: write. Neither started an
// attempt, so the task must come back rather than leave the job failed.
func TestARerunGitHubRefusedIsRetried(t *testing.T) {
	code, _ := failedJob(t, 1, &github{rerunStatus: http.StatusForbidden, runAttempt: 1}, jobStart.Add(5*time.Minute))
	if code != http.StatusInternalServerError {
		t.Errorf("status %d, want 500 so Cloud Tasks retries", code)
	}
}

// A successful job never needs the operations lookup, and a failed job that
// never reached a runner has no VM preemption to look for.
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
