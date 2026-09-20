package function

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// What GCE answers for an image that no longer exists. It names the rejected
// field rather than reporting RESOURCE_NOT_FOUND, which is why it used to read
// as retryable.
const deletedImageError = `googleapi: Error 400: Invalid value for field ` +
	`'resource.disks[0].initializeParams.sourceImage': ` +
	`'projects/p/global/images/gcrunner-ubuntu2404-x64-baf37b4-20260917131151'. ` +
	`The referenced image resource cannot be found., invalid`

const missingMachineTypeError = `googleapi: Error 400: Invalid value for field 'resource.machineType': ` +
	`'zones/europe-west3-a/machineTypes/c3-standard-4'. ` +
	`Machine type with name 'c3-standard-4' does not exist in zone 'europe-west3-a'., invalid`

// provision creates a VM across a three-zone pool, with fail naming the zones
// that reject it, and reports the zones tried and the final error.
func provision(t *testing.T, fail map[string]error) ([]string, error) {
	t.Helper()
	t.Setenv("GCRUNNER_ZONES", "europe-west3-a,europe-west1-b,us-central1-a")
	t.Setenv("GCE_REGION", "europe-west1")
	zoneOffset.Store(0)

	var tried []string
	withInsert(t, func(_ context.Context, _, zone, _ string, _ *RunnerLabels, _, _ string) error {
		tried = append(tried, zone)
		return fail[zone]
	})
	labels := &RunnerLabels{Machine: "c3-standard-4", MachineMode: "exact"}
	return tried, createRunnerInstance(context.Background(), labels, "vm", "jit", "owner", "repo")
}

// A deleted image gets the same answer in every zone and on every retry, so
// walking the pool and handing the task back to Cloud Tasks only queues work
// that cannot succeed ahead of work that can.
func TestADeletedImageIsTriedOnceAndNotRetried(t *testing.T) {
	deleted := errors.New(deletedImageError)
	tried, err := provision(t, map[string]error{"europe-west3-a": deleted, "europe-west1-b": deleted, "us-central1-a": deleted})
	if len(tried) != 1 {
		t.Errorf("tried %v, want one zone", tried)
	}
	if !errors.Is(err, errPermanent) {
		t.Errorf("not marked permanent, so Cloud Tasks would retry it: %v", err)
	}
}

// Machine types are zonal, so the same 400 for one missing from a zone must
// move the walk on rather than give up on the job.
func TestAMachineTypeMissingInOneZoneMovesToTheNext(t *testing.T) {
	tried, err := provision(t, map[string]error{"europe-west3-a": errors.New(missingMachineTypeError)})
	if err != nil {
		t.Fatalf("expected the second zone to succeed: %v", err)
	}
	if len(tried) != 2 {
		t.Errorf("tried %v, want two zones", tried)
	}
}

// queuedTask posts a queued job to /task/queued as Cloud Tasks does, with VM
// provisioning failing as given, and reports the status code.
func queuedTask(t *testing.T, provision error) int {
	t.Helper()
	originalProvision, originalFetch := provisionVM, fetchRepositoryFile
	t.Cleanup(func() {
		provisionVM, fetchRepositoryFile = originalProvision, originalFetch
		configCache.configs = map[string]configCacheEntry{}
	})
	configCache.configs = map[string]configCacheEntry{}
	fetchRepositoryFile = func(_ context.Context, _, _, _, _ string) ([]byte, error) { return nil, errConfigNotFound }
	provisionVM = func(_ context.Context, _ WorkflowJobEvent, _ *RunnerLabels) error { return provision }

	body, err := json.Marshal(WorkflowJobEvent{
		Action:      "queued",
		WorkflowJob: WorkflowJob{ID: 42, RunID: 7, Labels: []string{"gcrunner=7/machine=n2-standard-2"}},
		Repository:  Repository{FullName: "acme/app", Name: "app", Owner: RepositoryOwner{Login: "acme"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/task/queued", strings.NewReader(string(body)))
	request.Header.Set("X-CloudTasks-TaskName", "task-42")
	response := httptest.NewRecorder()
	HandleTask(response, request)
	return response.Code
}

// Cloud Tasks retries anything but a 2xx, so a permanent failure is
// acknowledged to drop the task, while any other failure is still handed back.
func TestQueuedTaskRetriesOnlyWhatCanSucceed(t *testing.T) {
	permanent := errors.Join(errPermanent, errors.New(deletedImageError))
	if code := queuedTask(t, permanent); code != http.StatusOK {
		t.Errorf("deleted image: status %d, want 200 so Cloud Tasks does not retry", code)
	}
	if code := queuedTask(t, errors.New("every zone out of quota")); code != http.StatusInternalServerError {
		t.Errorf("quota: status %d, want 500 so Cloud Tasks retries", code)
	}
}
