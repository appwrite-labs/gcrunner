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
// installation tokens, answers the rerun request with rerunStatus, lists the
// run's latest jobs as latest, by name and attempt, and records every rerun
// it was asked for.
type github struct {
	t           *testing.T
	rerunStatus int
	latest      map[string]int
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
	case req.URL.Path == "/repos/appwrite-labs/cloud/actions/runs/7/jobs" && req.URL.Query().Get("filter") == "latest":
		var jobs []string
		for name, attempt := range g.latest {
			jobs = append(jobs, fmt.Sprintf(`{"name": %q, "run_attempt": %d}`, name, attempt))
		}
		return answer(http.StatusOK, fmt.Sprintf(`{"jobs": [%s]}`, strings.Join(jobs, ",")))
	}
	g.t.Errorf("unexpected GitHub request %s %s", req.Method, req.URL.Path)
	return answer(http.StatusNotFound, `{}`)
}

// operations stands in for the Compute Engine operations API: a project whose
// recorded preemptions are those of gcrunner-7-42 at the given times, plus
// one of another VM in the middle of the job window. It accepts only the
// filter forms the real API does.
func operations(t *testing.T, preemptedAt ...time.Time) *httptest.Server {
	t.Helper()
	operation := func(instance string, at time.Time) string {
		return fmt.Sprintf(`{"operationType": "compute.instances.preempted", "status": "DONE", `+
			`"targetLink": "https://www.googleapis.com/compute/v1/projects/p/zones/us-central1-a/instances/%s", `+
			`"insertTime": %q}`, instance, at.Format(time.RFC3339Nano))
	}
	operations := []string{operation("gcrunner-7-41", jobStart.Add(10*time.Minute))}
	for _, at := range preemptedAt {
		operations = append(operations, operation("gcrunner-7-42", at))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/projects/p/aggregated/operations") {
			t.Errorf("unexpected Compute request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
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
// rerun, and the job already running on the next attempt shows the first one
// took, so the task is acknowledged rather than retried forever.
func TestARerunGitHubAlreadyStartedIsDone(t *testing.T) {
	gh := &github{rerunStatus: http.StatusForbidden, latest: map[string]int{"build": 2, "lint": 1}}
	code, _ := failedJob(t, 1, gh, jobStart.Add(5*time.Minute))
	if code != http.StatusOK {
		t.Errorf("status %d, want 200 so Cloud Tasks drops the task", code)
	}
}

// GitHub refuses a rerun while the run's other jobs are still going, and
// refuses it for good when the App lacks actions: write. Neither started an
// attempt, so the task must come back rather than leave the job failed.
func TestARerunGitHubRefusedIsRetried(t *testing.T) {
	gh := &github{rerunStatus: http.StatusForbidden, latest: map[string]int{"build": 1, "lint": 1}}
	code, _ := failedJob(t, 1, gh, jobStart.Add(5*time.Minute))
	if code != http.StatusInternalServerError {
		t.Errorf("status %d, want 500 so Cloud Tasks retries", code)
	}
}

// Two jobs of one run preempted together: rerunning the first moves the run
// to its next attempt, and GitHub refuses the second while that attempt runs.
// The run having advanced proves nothing about the second job, which is still
// on its first attempt, so its task must come back rather than be dropped.
func TestASiblingsRerunDoesNotCountForThisJob(t *testing.T) {
	gh := &github{rerunStatus: http.StatusForbidden, latest: map[string]int{"build": 1, "lint": 2}}
	code, _ := failedJob(t, 1, gh, jobStart.Add(5*time.Minute))
	if code != http.StatusInternalServerError {
		t.Errorf("status %d, want 500 so the job is rerun once the run settles", code)
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
