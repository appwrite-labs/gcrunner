package function

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
)

var (
	smClient     *secretmanager.Client
	smClientErr  error
	smClientOnce sync.Once
	projectID    string
)

// getSecretManagerClient creates the client once. The error is kept with it:
// a first attempt that fails used to leave a nil client behind, which every
// later call then dereferenced.
func getSecretManagerClient(ctx context.Context) (*secretmanager.Client, error) {
	smClientOnce.Do(func() {
		smClient, smClientErr = secretmanager.NewClient(ctx)
		projectID = os.Getenv("GCP_PROJECT")
		if projectID == "" {
			projectID = os.Getenv("GOOGLE_CLOUD_PROJECT")
		}
	})
	return smClient, smClientErr
}

func getSecret(ctx context.Context, name string) (string, error) {
	client, err := getSecretManagerClient(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to create secret manager client: %w", err)
	}

	result, err := client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
		Name: fmt.Sprintf("projects/%s/secrets/%s/versions/latest", projectID, name),
	})
	if err != nil {
		return "", fmt.Errorf("failed to access secret %s: %w", name, err)
	}

	return strings.TrimSpace(string(result.Payload.Data)), nil
}

// writeSecret creates a secret if it doesn't exist and adds a new version with the given value.
func writeSecret(ctx context.Context, name, value string) error {
	client, err := getSecretManagerClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to create secret manager client: %w", err)
	}

	secretName := fmt.Sprintf("projects/%s/secrets/%s", projectID, name)

	// Create secret if it doesn't exist
	_, err = client.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent:   fmt.Sprintf("projects/%s", projectID),
		SecretId: name,
		Secret: &secretmanagerpb.Secret{
			Replication: &secretmanagerpb.Replication{
				Replication: &secretmanagerpb.Replication_Automatic_{
					Automatic: &secretmanagerpb.Replication_Automatic{},
				},
			},
		},
	})
	if err != nil && !strings.Contains(err.Error(), "AlreadyExists") {
		return fmt.Errorf("failed to create secret %s: %w", name, err)
	}

	// Add new version
	_, err = client.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent: secretName,
		Payload: &secretmanagerpb.SecretPayload{
			Data: []byte(value),
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add secret version for %s: %w", name, err)
	}

	return nil
}

// HandleWebhook is the HTTP entry point that routes between
// setup pages and webhook handling.
func HandleWebhook(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/setup":
		handleSetup(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/setup/callback":
		handleSetupCallback(w, r)
	default:
		handleWebhook(w, r)
	}
}

// generateState creates an HMAC-signed state token containing a timestamp.
// The state is verified on callback to prevent CSRF without needing server-side storage.
func generateState(setupToken string) (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	nonceHex := hex.EncodeToString(nonce)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	payload := nonceHex + "." + ts
	mac := hmac.New(sha256.New, []byte(setupToken))
	mac.Write([]byte(payload))
	sig := hex.EncodeToString(mac.Sum(nil))
	return payload + "." + sig, nil
}

// verifyState checks that a state token is valid and not expired (1 hour TTL).
func verifyState(state, setupToken string) bool {
	parts := strings.SplitN(state, ".", 3)
	if len(parts) != 3 {
		return false
	}
	payload := parts[0] + "." + parts[1]
	sig, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(setupToken))
	mac.Write([]byte(payload))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return false
	}
	ts, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return false
	}
	return time.Since(time.Unix(ts, 0)) < 1*time.Hour
}

func handleSetup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Validate setup token
	setupToken, err := getSecret(ctx, "gcrunner-setup-token")
	if err != nil || setupToken == "" {
		log.Printf("ERROR: could not load setup token: %v", err)
		http.Error(w, "setup not available", http.StatusInternalServerError)
		return
	}
	providedToken := r.URL.Query().Get("token")
	if !hmac.Equal([]byte(providedToken), []byte(setupToken)) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Generate CSRF state parameter
	state, err := generateState(setupToken)
	if err != nil {
		log.Printf("ERROR: could not generate state: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	functionURL := fmt.Sprintf("https://%s", r.Host)

	manifest := GitHubAppManifest{
		Name: "gcrunner",
		URL:  "https://github.com/camdenclark/gcrunner",
		HookAttributes: map[string]string{
			"url": functionURL,
		},
		RedirectURL: functionURL + "/setup/callback",
		Public:      false,
		DefaultPermissions: map[string]string{
			"actions":        "write",
			"administration": "write",
			"checks":         "write",
			"contents":       "read",
		},
		DefaultEvents: []string{"workflow_job"},
	}

	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		log.Printf("ERROR: failed to marshal manifest: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>gcrunner — Setup</title>
  <style>
    *, *::before, *::after { box-sizing: border-box; }
    body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; background: #f6f8fa; color: #24292f; margin: 0; display: flex; align-items: center; justify-content: center; min-height: 100vh; }
    .card { background: #fff; border: 1px solid #d0d7de; border-radius: 12px; padding: 2.5rem; max-width: 480px; width: 100%%; margin: 1rem; box-shadow: 0 1px 3px rgba(0,0,0,0.04); }
    .logo { font-size: 1.5rem; font-weight: 700; margin-bottom: 0.25rem; }
    .logo span { color: #0969da; }
    .subtitle { color: #656d76; margin: 0 0 1.5rem; font-size: 0.95rem; }
    .btn { display: inline-block; background: #2da44e; color: #fff; border: none; border-radius: 6px; padding: 0.75rem 1.5rem; font-size: 1rem; font-weight: 600; cursor: pointer; transition: background 0.15s; }
    .btn:hover { background: #218838; }
  </style>
</head>
<body>
  <div class="card">
    <div class="logo"><span>gc</span>runner</div>
    <p class="subtitle">Create a GitHub App to connect gcrunner to your repositories.</p>
    <form action="https://github.com/settings/apps/new" method="post">
      <input type="hidden" name="manifest" value='%s'>
      <input type="hidden" name="state" value="%s">
      <button type="submit" class="btn">Create GitHub App</button>
    </form>
  </div>
</body>
</html>`, string(manifestJSON), state)
}

func handleSetupCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code parameter", http.StatusBadRequest)
		return
	}

	// Verify CSRF state parameter
	state := r.URL.Query().Get("state")
	setupToken, err := getSecret(ctx, "gcrunner-setup-token")
	if err != nil || setupToken == "" {
		log.Printf("ERROR: could not load setup token for state verification: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if state == "" || !verifyState(state, setupToken) {
		log.Printf("Setup callback: invalid or expired state parameter")
		http.Error(w, "invalid or expired state", http.StatusForbidden)
		return
	}

	// Exchange the code for app credentials
	resp, err := http.Post(
		fmt.Sprintf("https://api.github.com/app-manifests/%s/conversions", code),
		"application/json",
		nil,
	)
	if err != nil {
		log.Printf("ERROR: GitHub API error: %v", err)
		http.Error(w, "failed to contact GitHub API", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("ERROR: failed to read GitHub response: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if resp.StatusCode != http.StatusCreated {
		log.Printf("ERROR: GitHub returned %d: %s", resp.StatusCode, string(body))
		http.Error(w, "GitHub did not accept the app manifest", http.StatusInternalServerError)
		return
	}

	var app GitHubAppResponse
	if err := json.Unmarshal(body, &app); err != nil {
		log.Printf("ERROR: failed to parse GitHub response: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Write credentials to Secret Manager
	secrets := map[string]string{
		"gcrunner-app-id":         fmt.Sprintf("%d", app.ID),
		"gcrunner-private-key":    app.PEM,
		"gcrunner-webhook-secret": app.WebhookSecret,
	}
	for name, value := range secrets {
		if err := writeSecret(ctx, name, value); err != nil {
			log.Printf("ERROR writing secret %s: %v", name, err)
			http.Error(w, "failed to save credentials", http.StatusInternalServerError)
			return
		}
		log.Printf("Wrote secret %s", name)
	}

	installURL := fmt.Sprintf("%s/installations/new", app.HTMLURL)
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>gcrunner — Setup Complete</title>
  <style>
    *, *::before, *::after { box-sizing: border-box; }
    body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; background: #f6f8fa; color: #24292f; margin: 0; display: flex; align-items: center; justify-content: center; min-height: 100vh; }
    .card { background: #fff; border: 1px solid #d0d7de; border-radius: 12px; padding: 2.5rem; max-width: 480px; width: 100%%; margin: 1rem; box-shadow: 0 1px 3px rgba(0,0,0,0.04); }
    .logo { font-size: 1.5rem; font-weight: 700; margin-bottom: 0.25rem; }
    .logo span { color: #0969da; }
    .success { color: #1a7f37; font-weight: 600; font-size: 1.1rem; margin: 0.5rem 0 1rem; }
    .detail { color: #656d76; font-size: 0.9rem; margin: 0.35rem 0; }
    .detail strong { color: #24292f; }
    .saved { background: #dafbe1; color: #1a7f37; border-radius: 6px; padding: 0.5rem 0.75rem; font-size: 0.85rem; margin: 1rem 0; }
    .btn { display: inline-block; background: #2da44e; color: #fff; text-decoration: none; border-radius: 6px; padding: 0.75rem 1.5rem; font-size: 1rem; font-weight: 600; transition: background 0.15s; }
    .btn:hover { background: #218838; }
  </style>
</head>
<body>
  <div class="card">
    <div class="logo"><span>gc</span>runner</div>
    <p class="success">GitHub App created</p>
    <p class="detail"><strong>App:</strong> %s</p>
    <p class="detail"><strong>App ID:</strong> %d</p>
    <p class="saved">Credentials saved to Secret Manager.</p>
    <a href="%s" class="btn">Install on your repositories &rarr;</a>
  </div>
</body>
</html>`, app.Name, app.ID, installURL)

	log.Printf("GitHub App created: %s (ID: %d)", app.Name, app.ID)
}

func handleWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	log.Printf("Received %s request from %s, event: %s", r.Method, r.RemoteAddr, r.Header.Get("X-GitHub-Event"))

	// Verify HMAC signature using secret from Secret Manager
	secret, err := loadSecret(ctx, "gcrunner-webhook-secret")
	if err != nil {
		log.Printf("ERROR: could not load webhook secret: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if secret == "" {
		log.Printf("ERROR: webhook secret is empty")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	sig := r.Header.Get("X-Hub-Signature-256")
	if !verifySignature(body, sig, secret) {
		log.Printf("Signature verification failed")
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	log.Printf("Signature verified")

	event := r.Header.Get("X-GitHub-Event")
	if event != "workflow_job" {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "ok")
		return
	}

	var payload WorkflowJobEvent
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// Counted here rather than in the task, which Cloud Tasks may run many
	// times for one job.
	if parseJobLabels(payload.WorkflowJob.Labels) != nil {
		recordJob(ctx, payload)
		defer flushMetrics(ctx)
	}

	var taskPath string
	switch payload.Action {
	case "queued":
		taskPath = "/task/queued"
	case "completed":
		taskPath = "/task/completed"
	default:
		// in_progress only feeds the metrics above.
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "ok")
		return
	}

	if err := enqueue(ctx, taskPath, body, payload.WorkflowJob.ID); err != nil {
		log.Printf("ERROR enqueuing task for job %d: %v", payload.WorkflowJob.ID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	log.Printf("Enqueued %s task for job %d", payload.Action, payload.WorkflowJob.ID)
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "ok")
}

// Indirected so tests can drive handleWebhook without Secret Manager or
// Cloud Tasks.
var (
	loadSecret = getSecret
	enqueue    = enqueueTask
	schedule   = scheduleTask
)

// HandleTask handles requests from Cloud Tasks at /task/* paths.
// Cloud Run IAM ensures only the tasks SA can invoke this endpoint.
// The X-CloudTasks-TaskName header is set automatically by Cloud Tasks.
func HandleTask(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Verify the request comes from Cloud Tasks
	if r.Header.Get("X-CloudTasks-TaskName") == "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	var payload WorkflowJobEvent
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	retryCount := r.Header.Get("X-CloudTasks-TaskRetryCount")
	log.Printf("Task %s: action=%s job=%d retry=%s", r.URL.Path, payload.Action, payload.WorkflowJob.ID, retryCount)

	task := path.Base(r.URL.Path)
	switch r.URL.Path {
	case "/task/queued":
		err = handleQueued(ctx, payload)
	case "/task/completed":
		err = handleCompleted(ctx, payload)
	case rerunPath:
		err = handleRerun(ctx, payload)
	case checkPath:
		provisioned, _ := strconv.Atoi(r.URL.Query().Get("provisioned"))
		err = handleCheck(ctx, payload, provisioned)
	default:
		http.Error(w, "unknown task path", http.StatusNotFound)
		return
	}

	defer flushMetrics(ctx)
	switch {
	case err == nil:
		recordTask(ctx, task, retryCount, taskOutcomeOK)
	case errors.Is(err, errPermanent):
		// Acknowledged, not retried: the next attempt fails the same way and
		// only holds a queue slot. The job stays queued on GitHub either way.
		log.Printf("Job %d: %v, not retrying", payload.WorkflowJob.ID, err)
		recordTask(ctx, task, retryCount, taskOutcomePermanent)
	default:
		log.Printf("ERROR handling %s task: %v", task, err)
		recordTask(ctx, task, retryCount, taskOutcomeError)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "ok")
}

func handleQueued(ctx context.Context, event WorkflowJobEvent) error {
	provisioned, err := provisionJob(ctx, event)
	if err != nil || !provisioned {
		return err
	}
	return scheduleCheck(ctx, event, 0)
}

// provisionJob creates the job's VM and reports whether it did.
func provisionJob(ctx context.Context, event WorkflowJobEvent) (bool, error) {
	job := parseJobLabels(event.WorkflowJob.Labels)
	if job == nil {
		log.Printf("Job %d: not a gcrunner job, skipping", event.WorkflowJob.ID)
		return false, nil
	}

	labels, err := resolveJob(ctx, event, job)
	if err == nil {
		log.Printf("Job %d: creating VM with labels %+v", event.WorkflowJob.ID, labels)
		err = provisionVM(ctx, event, labels)
	}
	if !errors.Is(err, errConfiguration) {
		return err == nil, err
	}
	// A retry cannot fix the workflow; a queued job cannot be failed, so the
	// commit gets a failed check and the run is cancelled.
	log.Printf("Job %d: %v, failing the run", event.WorkflowJob.ID, err)
	if err := failRun(ctx, event.Repository.Owner.Login, event.Repository.Name, event.WorkflowJob, err); err != nil {
		if !isForbidden(err) {
			return false, fmt.Errorf("fail run for job %d: %w", event.WorkflowJob.ID, err)
		}
		log.Printf("Job %d: could not fail the run: %v", event.WorkflowJob.ID, err)
	}
	return false, nil
}

const (
	checkPath  = "/task/check"
	checkDelay = 5 * time.Minute
	// reprovisions caps the runners a check creates for one job.
	reprovisions = 3
)

// handleCheck replaces the runner of a job still queued after its VM is gone.
func handleCheck(ctx context.Context, event WorkflowJobEvent, provisioned int) error {
	owner, repo, id := event.Repository.Owner.Login, event.Repository.Name, event.WorkflowJob.ID
	status, err := jobStatus(ctx, owner, repo, id)
	if err != nil {
		return fmt.Errorf("check job %d: %w", id, err)
	}
	if status != jobStatusQueued {
		return nil
	}
	name := fmt.Sprintf("gcrunner-%d-%d", event.WorkflowJob.RunID, id)
	zone, err := findZone(ctx, name)
	if err != nil {
		return err
	}
	if zone == "" {
		if provisioned >= reprovisions {
			log.Printf("Job %d: still queued after %d replacement runners, giving up", id, provisioned)
			return nil
		}
		log.Printf("Job %d: still queued and VM %s is gone, replacing its runner", id, name)
		created, err := provisionJob(ctx, event)
		if err != nil || !created {
			return err
		}
		provisioned++
	}
	return scheduleCheck(ctx, event, provisioned)
}

// scheduleCheck schedules handleCheck for later, named by its due time like deferRerun.
func scheduleCheck(ctx context.Context, event WorkflowJobEvent, provisioned int) error {
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode job %d: %w", event.WorkflowJob.ID, err)
	}
	at := time.Now().Add(checkDelay)
	name := fmt.Sprintf("job-%d-check-%d", event.WorkflowJob.ID, at.Unix())
	if err := schedule(ctx, fmt.Sprintf("%s?provisioned=%d", checkPath, provisioned), body, name, at); err != nil {
		return fmt.Errorf("schedule check of job %d: %w", event.WorkflowJob.ID, err)
	}
	return nil
}

// Indirected so tests can drive HandleTask without Compute or GitHub.
var (
	provisionVM = createRunnerVM
	findZone    = findInstanceZone
)

// resolveJob turns the job's labels into a runner using the repository's config.
func resolveJob(ctx context.Context, event WorkflowJobEvent, job *JobLabels) (*RunnerLabels, error) {
	config, err := loadRepositoryConfig(ctx, event)
	if err != nil {
		return nil, err
	}
	return config.resolve(job, imageProject())
}

// Indirected so tests can drive handleCompleted without Compute or GitHub.
var (
	deleteVM     = deleteRunnerVM
	runnerIsBusy = runnerBusy
)

// rerunAttempts caps reruns after preemption, counting manual attempts too.
const rerunAttempts = 3

func handleCompleted(ctx context.Context, event WorkflowJobEvent) error {
	if parseJobLabels(event.WorkflowJob.Labels) == nil {
		return nil
	}

	instanceName, ranHere := deletionTarget(event)
	deferred := false
	if ranHere && event.WorkflowJob.Conclusion == conclusionFailure {
		err := rerunIfPreempted(ctx, event, instanceName)
		deferred = errors.Is(err, errRunBusy)
		if err != nil && !deferred {
			return err
		}
	}
	if !ranHere {
		// The job never reached a runner. Its own VM may meanwhile have picked
		// up another job with the same labels, so only delete it when idle.
		busy, err := runnerIsBusy(ctx, event.Repository.Owner.Login, event.Repository.Name, instanceName)
		if err != nil {
			return fmt.Errorf("check runner %s: %w", instanceName, err)
		}
		if busy {
			log.Printf("Job %d: completed without running, VM %s is busy with another job, leaving it", event.WorkflowJob.ID, instanceName)
			return nil
		}
	}
	log.Printf("Job %d: completed, deleting VM %s", event.WorkflowJob.ID, instanceName)
	if err := deleteVM(ctx, instanceName); err != nil || !deferred {
		return err
	}
	return deferRerun(ctx, event)
}

const (
	rerunPath  = "/task/rerun"
	rerunDelay = 5 * time.Minute
)

// handleRerun reruns a preempted job once the rest of its run has finished.
func handleRerun(ctx context.Context, event WorkflowJobEvent) error {
	err := rerunWorkflowJob(ctx, event.Repository.Owner.Login, event.Repository.Name, event.WorkflowJob)
	if errors.Is(err, errRunBusy) {
		return deferRerun(ctx, event)
	}
	return err
}

// deferRerun schedules handleRerun for later. Cloud Tasks will not reuse a task
// name for hours, so the name carries the due time.
func deferRerun(ctx context.Context, event WorkflowJobEvent) error {
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode job %d: %w", event.WorkflowJob.ID, err)
	}
	at := time.Now().Add(rerunDelay)
	name := fmt.Sprintf("job-%d-rerun-%d", event.WorkflowJob.ID, at.Unix())
	if err := schedule(ctx, rerunPath, body, name, at); err != nil {
		return fmt.Errorf("defer rerun of job %d: %w", event.WorkflowJob.ID, err)
	}
	log.Printf("Job %d: run still in progress, checking again in %s", event.WorkflowJob.ID, rerunDelay)
	return nil
}

// rerunIfPreempted re-queues a failed job whose spot VM was preempted while
// the job ran. Preemption deletes the VM outright, so the preempted operation
// on the instance is the only trace, and a real failure never produces one.
func rerunIfPreempted(ctx context.Context, event WorkflowJobEvent, instanceName string) error {
	job := event.WorkflowJob
	preempted, err := instancePreempted(ctx, instanceName, job.StartedAt, job.CompletedAt)
	if err != nil {
		return fmt.Errorf("check preemption of %s: %w", instanceName, err)
	}
	if !preempted {
		return nil
	}
	ran := job.CompletedAt.Sub(job.StartedAt).Round(time.Second)
	if job.RunAttempt >= rerunAttempts {
		log.Printf("Job %d: preempted after %s on attempt %d, not rerunning past attempt %d", job.ID, ran, job.RunAttempt, rerunAttempts)
		return nil
	}
	log.Printf("Job %d: preempted after %s, rerunning (attempt %d)", job.ID, ran, job.RunAttempt+1)
	if err := rerunWorkflowJob(ctx, event.Repository.Owner.Login, event.Repository.Name, job); err != nil {
		return fmt.Errorf("rerun job %d: %w", job.ID, err)
	}
	return nil
}

// deletionTarget names the VM to delete for a completed job. GitHub hands a
// queued job to any idle runner whose labels cover it, so the VM created for
// this job may have run a different job; runner_name is the one that ran this
// job. The bool reports whether the job actually ran on a gcrunner VM.
func deletionTarget(event WorkflowJobEvent) (string, bool) {
	if strings.HasPrefix(event.WorkflowJob.RunnerName, "gcrunner-") {
		return event.WorkflowJob.RunnerName, true
	}
	return fmt.Sprintf("gcrunner-%d-%d", event.WorkflowJob.RunID, event.WorkflowJob.ID), false
}

func verifySignature(payload []byte, signature, secret string) bool {
	if !strings.HasPrefix(signature, "sha256=") {
		return false
	}
	sig, err := hex.DecodeString(signature[7:])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	expected := mac.Sum(nil)
	return hmac.Equal(sig, expected)
}

// WorkflowJobEvent represents a GitHub workflow_job webhook payload.
type WorkflowJobEvent struct {
	Action      string      `json:"action"`
	WorkflowJob WorkflowJob `json:"workflow_job"`
	Repository  Repository  `json:"repository"`
}

type WorkflowJob struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	RunID        int64     `json:"run_id"`
	HeadSHA      string    `json:"head_sha"`
	Labels       []string  `json:"labels"`
	RunAttempt   int       `json:"run_attempt"`
	RunnerName   string    `json:"runner_name"`
	WorkflowName string    `json:"workflow_name"`
	Conclusion   string    `json:"conclusion"`
	CreatedAt    time.Time `json:"created_at"`
	StartedAt    time.Time `json:"started_at"`
	CompletedAt  time.Time `json:"completed_at"`
}

const conclusionFailure = "failure"

type Repository struct {
	FullName      string          `json:"full_name"`
	Owner         RepositoryOwner `json:"owner"`
	Name          string          `json:"name"`
	Private       bool            `json:"private"`
	DefaultBranch string          `json:"default_branch"`
}

type RepositoryOwner struct {
	Login string `json:"login"`
}

// GitHubAppManifest is the manifest sent to GitHub to create a new App.
type GitHubAppManifest struct {
	Name               string            `json:"name"`
	URL                string            `json:"url"`
	HookAttributes     map[string]string `json:"hook_attributes"`
	RedirectURL        string            `json:"redirect_url"`
	Public             bool              `json:"public"`
	DefaultPermissions map[string]string `json:"default_permissions"`
	DefaultEvents      []string          `json:"default_events"`
}

// GitHubAppResponse is the response from the manifest conversion endpoint.
type GitHubAppResponse struct {
	ID            int    `json:"id"`
	Slug          string `json:"slug"`
	Name          string `json:"name"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	WebhookSecret string `json:"webhook_secret"`
	PEM           string `json:"pem"`
	HTMLURL       string `json:"html_url"`
}
