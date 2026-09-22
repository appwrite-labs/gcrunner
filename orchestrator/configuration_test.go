package function

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/option"
)

// engine is what Compute Engine answers while a job is provisioned: the
// zones discovery reports (nil for a discovery failure), each zone's
// machine catalogue (a missing zone cannot be read), and each zone's answer
// to an insert.
type engine struct {
	zones     []string
	catalogue map[string][]*MachineTypeInfo
	insert    map[string]error
}

var fourVCPUs = []*MachineTypeInfo{{Name: "c3d-highcpu-4", Family: "c3d", VCPUs: 4, MemoryMB: 8192}}

// queuedJob delivers the queued task for job "manifest" of run 7 asking for
// preset "deploy", with the repository's config file and Compute as given,
// and reports the status handed back.
func queuedJob(t *testing.T, gh *github, config string, gce engine) int {
	t.Helper()
	t.Setenv("GCRUNNER_ZONES", "")
	t.Setenv("GCE_REGION", "europe-west3")
	t.Setenv("GCP_PROJECT", "p")
	useGitHub(t, gh)
	originalFetch, originalZones, originalOptions := fetchRepositoryFile, listZones, computeOptions
	t.Cleanup(func() {
		fetchRepositoryFile, listZones, computeOptions = originalFetch, originalZones, originalOptions
		configCache.configs = map[string]configCacheEntry{}
		zoneCache.zones = map[string]zoneCacheEntry{}
		machineTypeCache.types = map[string]machineTypeCacheEntry{}
	})
	configCache.configs = map[string]configCacheEntry{}
	zoneCache.zones = map[string]zoneCacheEntry{}
	machineTypeCache.types = map[string]machineTypeCacheEntry{}
	zoneOffset.Store(0)
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"items": {}}`) }))
	t.Cleanup(empty.Close)
	computeOptions = []option.ClientOption{option.WithEndpoint(empty.URL), option.WithoutAuthentication()}
	fetchRepositoryFile = func(context.Context, string, string, string, string) ([]byte, error) { return []byte(config), nil }
	listZones = func(context.Context, string, string) ([]string, error) {
		if gce.zones == nil {
			return nil, errors.New("compute unavailable")
		}
		return gce.zones, nil
	}
	withMachineTypes(t, func(_ context.Context, _, zone string) ([]*MachineTypeInfo, error) {
		if types, ok := gce.catalogue[zone]; ok {
			return types, nil
		}
		return nil, errors.New("compute unavailable in " + zone)
	})
	withInsert(t, func(_ context.Context, zone string, _ *computepb.Instance) error { return gce.insert[zone] })
	return deliverTask(t, "/task/queued", WorkflowJobEvent{
		Action:      "queued",
		WorkflowJob: WorkflowJob{ID: 42, Name: "manifest", RunID: 7, HeadSHA: "6d8b388", Labels: []string{"gcrunner=7/runner=deploy"}},
		Repository:  cloudRepository,
	})
}

const (
	twoVCPUsConfig  = "runners:\n  deploy:\n    family: c3d\n    cpu: 2\n    ram: 8\n"
	exactNameConfig = "runners:\n  deploy:\n    machine: n2d-standard-999\n"
)

func TestAJobThatCanNeverStartFailsItsRunOnce(t *testing.T) {
	missing := errors.New(missingMachineTypeError)
	two := engine{zones: []string{"europe-west3-a", "europe-west3-b"}, catalogue: map[string][]*MachineTypeInfo{"europe-west3-a": fourVCPUs, "europe-west3-b": fourVCPUs}}
	for name, c := range map[string]struct {
		config string
		gce    engine
		reason string
	}{
		"preset not defined":            {"runners:\n  build:\n    cpu: 4\n", two, `runner "deploy" is not defined`},
		"no zone offers the size":       {twoVCPUsConfig, two, "no machine type matching"},
		"no zone offers the exact name": {exactNameConfig, engine{zones: two.zones, insert: map[string]error{"europe-west3-a": missing, "europe-west3-b": missing}}, "does not exist"},
	} {
		gh := &github{checkStatus: http.StatusCreated}
		if code := queuedJob(t, gh, c.config, c.gce); code != http.StatusOK {
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

// Only Compute answering in every zone proves the workflow wrong. A zone
// that cannot be read, a zone out of quota, guessed zones after discovery
// failed, and GitHub itself being down all leave the task to be retried.
func TestATransientFailureIsRetriedWithoutFailingTheRun(t *testing.T) {
	missing := errors.New(missingMachineTypeError)
	zones := []string{"europe-west3-a", "europe-west3-b"}
	for name, c := range map[string]struct {
		config string
		gce    engine
		check  int
	}{
		"one catalogue unreadable":  {twoVCPUsConfig, engine{zones: zones, catalogue: map[string][]*MachineTypeInfo{"europe-west3-a": fourVCPUs}}, http.StatusCreated},
		"one zone out of quota":     {exactNameConfig, engine{zones: zones, insert: map[string]error{"europe-west3-a": missing, "europe-west3-b": errors.New("QUOTA_EXCEEDED")}}, http.StatusCreated},
		"zones guessed":             {twoVCPUsConfig, engine{catalogue: map[string][]*MachineTypeInfo{"europe-west3-a": fourVCPUs, "europe-west3-b": fourVCPUs, "europe-west3-c": fourVCPUs}}, http.StatusCreated},
		"GitHub down for the check": {twoVCPUsConfig, engine{zones: zones, catalogue: map[string][]*MachineTypeInfo{"europe-west3-a": fourVCPUs, "europe-west3-b": fourVCPUs}}, http.StatusBadGateway},
	} {
		gh := &github{checkStatus: c.check}
		if code := queuedJob(t, gh, c.config, c.gce); code != http.StatusInternalServerError {
			t.Errorf("%s: status %d, want 500 so Cloud Tasks retries", name, code)
		}
		if gh.cancels != 0 {
			t.Errorf("%s: cancelled the run", name)
		}
	}
}

// Without checks: write the report cannot be made, but retrying would not
// grant it, so the task is still acknowledged.
func TestARefusedCheckRunIsNotRetried(t *testing.T) {
	gh := &github{checkStatus: http.StatusForbidden}
	if code := queuedJob(t, gh, "runners: {}\n", engine{}); code != http.StatusOK {
		t.Errorf("status %d, want 200", code)
	}
	if gh.cancels != 0 {
		t.Errorf("cancelled the run without having explained why")
	}
}

func withMachineTypes(t *testing.T, fn func(ctx context.Context, project, zone string) ([]*MachineTypeInfo, error)) {
	t.Helper()
	original := listMachineTypes
	listMachineTypes = fn
	t.Cleanup(func() { listMachineTypes = original })
}
