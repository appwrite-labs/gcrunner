package function

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// collectMetrics installs an in-memory metrics pipeline for the test and
// returns a function that reads back everything recorded so far.
func collectMetrics(t *testing.T) func() metricdata.ResourceMetrics {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	telemetryMu.Lock()
	original := telemetryInst
	telemetryInst = withProvider(provider)
	telemetryMu.Unlock()
	t.Cleanup(func() {
		telemetryMu.Lock()
		telemetryInst = original
		telemetryMu.Unlock()
	})
	return func() metricdata.ResourceMetrics {
		var collected metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &collected); err != nil {
			t.Fatal(err)
		}
		return collected
	}
}

// counter returns the value of the named counter's series with exactly these
// attributes, or 0 when nothing was recorded for it.
func counter(t *testing.T, collected metricdata.ResourceMetrics, name string, attributes ...attribute.KeyValue) int64 {
	t.Helper()
	want := attribute.NewSet(attributes...)
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want a counter", name, m.Data)
			}
			for _, point := range sum.DataPoints {
				if point.Attributes.Equals(&want) {
					return point.Value
				}
			}
		}
	}
	return 0
}

// histogram returns the count and sum of the named histogram's series with
// exactly these attributes.
func histogram(t *testing.T, collected metricdata.ResourceMetrics, name string, attributes ...attribute.KeyValue) (uint64, float64) {
	t.Helper()
	want := attribute.NewSet(attributes...)
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			switch data := m.Data.(type) {
			case metricdata.Histogram[float64]:
				for _, point := range data.DataPoints {
					if point.Attributes.Equals(&want) {
						return point.Count, point.Sum
					}
				}
			case metricdata.Histogram[int64]:
				for _, point := range data.DataPoints {
					if point.Attributes.Equals(&want) {
						return point.Count, float64(point.Sum)
					}
				}
			default:
				t.Fatalf("%s is %T, want a histogram", name, m.Data)
			}
		}
	}
	return 0, 0
}

// deliverWebhook posts a signed workflow_job webhook as GitHub does, with
// Secret Manager and Cloud Tasks stood in for, and reports the status code.
func deliverWebhook(t *testing.T, event WorkflowJobEvent) int {
	t.Helper()
	const secret = "webhook-secret"
	originalSecret, originalEnqueue := loadSecret, enqueue
	t.Cleanup(func() { loadSecret, enqueue = originalSecret, originalEnqueue })
	loadSecret = func(_ context.Context, _ string) (string, error) { return secret, nil }
	enqueue = func(_ context.Context, _ string, _ []byte, _ int64) error { return nil }

	body, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	request.Header.Set("X-GitHub-Event", "workflow_job")
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	response := httptest.NewRecorder()
	HandleWebhook(response, request)
	return response.Code
}

func TestJobWebhooksAreCountedWithTheirTimings(t *testing.T) {
	collect := collectMetrics(t)
	created := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	job := WorkflowJob{ID: 42, RunID: 7, Labels: gcrunnerLabels, WorkflowName: "CI",
		CreatedAt: created, StartedAt: created.Add(90 * time.Second), CompletedAt: created.Add(690 * time.Second)}
	repository := Repository{FullName: "appwrite-labs/cloud", Name: "cloud", Owner: RepositoryOwner{Login: "appwrite-labs"}}

	for _, action := range []string{"queued", "in_progress", "completed"} {
		if action == "completed" {
			job.Conclusion = "failure"
		}
		if code := deliverWebhook(t, WorkflowJobEvent{Action: action, WorkflowJob: job, Repository: repository}); code != http.StatusOK {
			t.Fatalf("%s webhook: status %d", action, code)
		}
	}
	other := WorkflowJob{ID: 43, RunID: 7, Labels: []string{"ubuntu-latest"}, WorkflowName: "CI", Conclusion: "success"}
	if code := deliverWebhook(t, WorkflowJobEvent{Action: "completed", WorkflowJob: other, Repository: repository}); code != http.StatusOK {
		t.Fatalf("other provider's webhook: status %d", code)
	}

	collected := collect()
	identity := []attribute.KeyValue{attribute.String("repo_full_name", "appwrite-labs/cloud"), attribute.String("workflow_name", "CI")}
	if got := counter(t, collected, "gcrunner.jobs", append(identity, attribute.String("status", "completed"), attribute.String("conclusion", "failure"))...); got != 1 {
		t.Errorf("completed failure jobs = %d, want 1", got)
	}
	if got := counter(t, collected, "gcrunner.jobs", append(identity, attribute.String("status", "queued"), attribute.String("conclusion", ""))...); got != 1 {
		t.Errorf("queued jobs = %d, want 1", got)
	}
	if got := counter(t, collected, "gcrunner.jobs", append(identity, attribute.String("status", "completed"), attribute.String("conclusion", "success"))...); got != 0 {
		t.Errorf("a job on another provider was counted: %d", got)
	}
	if count, sum := histogram(t, collected, "gcrunner.queue.duration", identity...); count != 1 || sum != 90 {
		t.Errorf("queue duration count=%d sum=%v, want one observation of 90s", count, sum)
	}
	if count, sum := histogram(t, collected, "gcrunner.job.duration", identity...); count != 1 || sum != 600 {
		t.Errorf("job duration count=%d sum=%v, want one observation of 600s", count, sum)
	}
}

func TestEveryZoneTriedReportsItsOutcome(t *testing.T) {
	collect := collectMetrics(t)
	quota := errors.New("googleapi: Error 403: QUOTA_EXCEEDED")
	tried, err := provision(t, map[string]error{"europe-west3-a": quota})
	if err != nil {
		t.Fatalf("expected a later zone to succeed: %v", err)
	}
	if len(tried) < 2 {
		t.Fatalf("tried %v, want the quota zone and at least one more", tried)
	}

	collected := collect()
	attempts := func(zone, outcome string) int64 {
		return counter(t, collected, "gcrunner.vm.creates",
			attribute.String("zone", zone), attribute.String("machine_type", "c3-standard-4"),
			attribute.Bool("spot", false), attribute.String("outcome", outcome))
	}
	// Every zone the walk visited reports once: the ones that refused as
	// quota, the one that accepted as created.
	last := len(tried) - 1
	for _, zone := range tried[:last] {
		if got := attempts(zone, "quota"); got != 1 {
			t.Errorf("%s quota attempts = %d, want 1", zone, got)
		}
	}
	if got := attempts(tried[last], "created"); got != 1 {
		t.Errorf("%s created attempts = %d, want 1", tried[last], got)
	}
	for _, zone := range configuredZones() {
		if slices.Contains(tried, zone) {
			continue
		}
		if got := attempts(zone, "created") + attempts(zone, "quota"); got != 0 {
			t.Errorf("%s was never tried but reports %d attempts", zone, got)
		}
	}
}

func TestTaskAttemptsRecordTheirRetryDepth(t *testing.T) {
	collect := collectMetrics(t)
	originalProvision, originalFetch := provisionVM, fetchRepositoryFile
	t.Cleanup(func() {
		provisionVM, fetchRepositoryFile = originalProvision, originalFetch
		configCache.configs = map[string]configCacheEntry{}
	})
	configCache.configs = map[string]configCacheEntry{}
	fetchRepositoryFile = func(_ context.Context, _, _, _, _ string) ([]byte, error) { return nil, errConfigNotFound }
	provisionVM = func(_ context.Context, _ WorkflowJobEvent, _ *RunnerLabels) error {
		return errors.New("every zone out of quota")
	}

	body, err := json.Marshal(WorkflowJobEvent{
		Action:      "queued",
		WorkflowJob: WorkflowJob{ID: 42, RunID: 7, Labels: gcrunnerLabels},
		Repository:  Repository{FullName: "acme/app", Name: "app", Owner: RepositoryOwner{Login: "acme"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/task/queued", strings.NewReader(string(body)))
	request.Header.Set("X-CloudTasks-TaskName", "task-42")
	request.Header.Set("X-CloudTasks-TaskRetryCount", "18")
	HandleTask(httptest.NewRecorder(), request)

	collected := collect()
	if got := counter(t, collected, "gcrunner.tasks", attribute.String("task", "queued"), attribute.String("outcome", "error")); got != 1 {
		t.Errorf("failed queued attempts = %d, want 1", got)
	}
	if count, sum := histogram(t, collected, "gcrunner.task.retries", attribute.String("task", "queued")); count != 1 || sum != 18 {
		t.Errorf("retry depth count=%d sum=%v, want one observation of 18", count, sum)
	}
}
