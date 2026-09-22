package function

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	computepb "cloud.google.com/go/compute/apiv1/computepb"
)

// queuedJob delivers the queued task for job "manifest" of run 7 asking for
// preset "deploy", with the repository's config file as given and VM
// provisioning failing with provision, and reports the status handed back.
func queuedJob(t *testing.T, gh *github, config string, provision error) int {
	t.Helper()
	useGitHub(t, gh)
	originalProvision, originalFetch := provisionVM, fetchRepositoryFile
	t.Cleanup(func() {
		provisionVM, fetchRepositoryFile = originalProvision, originalFetch
		configCache.configs = map[string]configCacheEntry{}
	})
	configCache.configs = map[string]configCacheEntry{}
	fetchRepositoryFile = func(context.Context, string, string, string, string) ([]byte, error) { return []byte(config), nil }
	provisionVM = func(context.Context, WorkflowJobEvent, *RunnerLabels) error { return provision }
	return deliverTask(t, "/task/queued", WorkflowJobEvent{
		Action:      "queued",
		WorkflowJob: WorkflowJob{ID: 42, Name: "manifest", RunID: 7, HeadSHA: "6d8b388", Labels: []string{"gcrunner=7/runner=deploy"}},
		Repository:  cloudRepository,
	})
}

const deployConfig = "runners:\n  deploy:\n    family: c3d\n    cpu: 2\n    ram: 8\n"

func TestAJobThatCanNeverStartFailsItsRunOnce(t *testing.T) {
	for name, c := range map[string]struct {
		config    string
		provision error
		reason    string
	}{
		"preset not defined":  {"runners:\n  build:\n    cpu: 4\n", nil, `runner "deploy" is not defined`},
		"no matching machine": {deployConfig, fmt.Errorf("%w: %w", errConfiguration, errNoMachineType), "no machine type"},
	} {
		gh := &github{checkStatus: http.StatusCreated}
		if code := queuedJob(t, gh, c.config, c.provision); code != http.StatusOK {
			t.Errorf("%s: status %d, want 200 so Cloud Tasks does not retry", name, code)
		}
		if len(gh.checks) != 1 || gh.cancels != 1 {
			t.Fatalf("%s: %d check runs and %d cancels, want one of each", name, len(gh.checks), gh.cancels)
		}
		check := gh.checks[0]
		output, _ := check["output"].(map[string]any)
		summary, _ := output["summary"].(string)
		if check["head_sha"] != "6d8b388" || check["conclusion"] != "failure" || output["title"] != "manifest cannot start" || !strings.Contains(summary, c.reason) {
			t.Errorf("%s: check run %v does not carry the reason %q", name, check, c.reason)
		}
	}
}

func TestATransientFailureIsRetriedWithoutFailingTheRun(t *testing.T) {
	gh := &github{checkStatus: http.StatusCreated}
	if code := queuedJob(t, gh, deployConfig, errors.New("every zone out of quota")); code != http.StatusInternalServerError {
		t.Errorf("status %d, want 500 so Cloud Tasks retries", code)
	}
	if len(gh.checks) != 0 || gh.cancels != 0 {
		t.Errorf("%d check runs and %d cancels, want none", len(gh.checks), gh.cancels)
	}
}

// Without checks: write the report cannot be made, but retrying would not
// grant it, so the task is still acknowledged.
func TestARefusedCheckRunIsNotRetried(t *testing.T) {
	gh := &github{checkStatus: http.StatusForbidden}
	if code := queuedJob(t, gh, "runners: {}\n", nil); code != http.StatusOK {
		t.Errorf("status %d, want 200", code)
	}
	if gh.cancels != 0 {
		t.Errorf("cancelled the run without having explained why")
	}
}

// A size no zone offers is the workflow's mistake; a zone whose catalogue
// cannot be read is not.
func TestAMachineSizeNoZoneOffersIsAConfigurationError(t *testing.T) {
	t.Setenv("GCRUNNER_ZONES", "europe-west3-a,us-east1-b")
	t.Setenv("GCE_REGION", "europe-west3")
	zoneOffset.Store(0)
	withInsert(t, func(context.Context, string, *computepb.Instance) error {
		t.Error("created a VM without a machine type")
		return nil
	})
	catalogue := func(zones ...string) {
		machineTypeCache.mu.Lock()
		defer machineTypeCache.mu.Unlock()
		machineTypeCache.types = map[string]machineTypeCacheEntry{}
		for _, zone := range zones {
			machineTypeCache.types[zone] = machineTypeCacheEntry{
				types:     []*MachineTypeInfo{{Name: "c3d-highcpu-4", Family: "c3d", VCPUs: 4, MemoryMB: 8192}},
				fetchedAt: time.Now(),
			}
		}
	}
	t.Cleanup(func() { catalogue() })
	labels := &RunnerLabels{MachineMode: machineModeFamily, Family: "c3d", CPU: "2", RAM: "8"}

	catalogue("europe-west3-a", "us-east1-b")
	err := createRunnerInstance(context.Background(), labels, "vm", "jit", "owner", "repo")
	if !errors.Is(err, errConfiguration) {
		t.Errorf("both zones lack the size: %v, want a configuration error", err)
	}

	catalogue("europe-west3-a")
	withMachineTypes(t, func(_ context.Context, _, zone string) ([]*MachineTypeInfo, error) {
		return nil, errors.New("compute unavailable in " + zone)
	})
	err = createRunnerInstance(context.Background(), labels, "vm", "jit", "owner", "repo")
	if errors.Is(err, errConfiguration) {
		t.Errorf("one zone unreadable: %v, want a retryable error", err)
	}
}

// An exact machine name skips the catalogue and is refused by the insert
// instead; missing from every zone, that is the workflow's mistake too.
func TestAnExactMachineNameNoZoneOffersIsAConfigurationError(t *testing.T) {
	missing := errors.New(missingMachineTypeError)
	_, err := provision(t, map[string]error{"europe-west3-a": missing, "europe-west1-b": missing, "us-central1-a": missing})
	if !errors.Is(err, errConfiguration) {
		t.Errorf("every zone refused the name: %v, want a configuration error", err)
	}
	_, err = provision(t, map[string]error{"europe-west3-a": missing, "europe-west1-b": errors.New("every zone out of quota"), "us-central1-a": missing})
	if errors.Is(err, errConfiguration) {
		t.Errorf("one zone failed for another reason: %v, want a retryable error", err)
	}
}

func withMachineTypes(t *testing.T, fn func(ctx context.Context, project, zone string) ([]*MachineTypeInfo, error)) {
	t.Helper()
	original := listMachineTypes
	listMachineTypes = fn
	t.Cleanup(func() { listMachineTypes = original })
}
