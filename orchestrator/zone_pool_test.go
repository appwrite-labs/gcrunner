package function

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func withInsert(t *testing.T, fn func(ctx context.Context, name, zone, machineType string, labels *RunnerLabels, startupScript, jitConfig string) error) {
	t.Helper()
	original := insertInstance
	insertInstance = fn
	zoneOffset.Store(0)
	t.Cleanup(func() { insertInstance = original })
}

// A zone that is out of quota must not end the attempt: the job belongs on
// whichever zone in the pool still has room.
func TestQuotaInOneZoneFallsThroughToTheNext(t *testing.T) {
	t.Setenv("GCRUNNER_ZONES", "zone-a,zone-b")
	var tried []string
	withInsert(t, func(_ context.Context, _, zone, _ string, _ *RunnerLabels, _, _ string) error {
		tried = append(tried, zone)
		if zone == "zone-a" {
			return errors.New("QUOTA_EXCEEDED: cpus")
		}
		return nil
	})

	labels := &RunnerLabels{Zone: "zone-a+zone-b", Machine: "n2-standard-2", MachineMode: "exact"}
	if err := createRunnerInstance(context.Background(), labels, "vm-1", "jit", "owner", "repo"); err != nil {
		t.Fatalf("expected the second zone to serve the job, got %v", err)
	}
	if len(tried) != 2 || tried[1] != "zone-b" {
		t.Fatalf("expected both zones to be tried in order, got %v", tried)
	}
}

// Zones pinned on the label are a preference order, not a pool: the first
// listed zone must be tried first on every job.
func TestLabelZonesAreNotRotated(t *testing.T) {
	var first []string
	withInsert(t, func(_ context.Context, _, zone, _ string, _ *RunnerLabels, _, _ string) error {
		first = append(first, zone)
		return nil
	})

	labels := &RunnerLabels{Zone: "zone-a+zone-b", Machine: "n2-standard-2", MachineMode: "exact"}
	for i := 0; i < 3; i++ {
		if err := createRunnerInstance(context.Background(), labels, "vm", "jit", "owner", "repo"); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	for i, zone := range first {
		if zone != "zone-a" {
			t.Fatalf("job %d started in %s, expected the first listed zone", i, zone)
		}
	}
}

// A stockout in one zone and quota in another is not "every zone out of
// quota": the last failure has to stay visible so operators see the real blocker.
func TestMixedQuotaAndStockoutKeepsBothErrors(t *testing.T) {
	withInsert(t, func(_ context.Context, _, zone, _ string, _ *RunnerLabels, _, _ string) error {
		if zone == "zone-a" {
			return errors.New("QUOTA_EXCEEDED: cpus")
		}
		return errors.New("ZONE_RESOURCE_POOL_EXHAUSTED")
	})

	labels := &RunnerLabels{Zone: "zone-a+zone-b", Machine: "n2-standard-2", MachineMode: "exact"}
	err := createRunnerInstance(context.Background(), labels, "vm-3", "jit", "owner", "repo")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "every zone out of quota") {
		t.Fatalf("mixed failure reported as quota everywhere: %v", err)
	}
	if !strings.Contains(err.Error(), "ZONE_RESOURCE_POOL_EXHAUSTED") || !strings.Contains(err.Error(), "zone-a") {
		t.Fatalf("expected the stockout error and the quota zone to surface, got %v", err)
	}
}

// Only when the whole pool is out of quota is there nothing to do but wait,
// and the caller has to see a quota error so Cloud Tasks retries.
func TestQuotaEverywhereReportsQuota(t *testing.T) {
	withInsert(t, func(_ context.Context, _, _, _ string, _ *RunnerLabels, _, _ string) error {
		return errors.New("QUOTA_EXCEEDED: cpus")
	})

	labels := &RunnerLabels{Zone: "zone-a+zone-b", Machine: "n2-standard-2", MachineMode: "exact"}
	err := createRunnerInstance(context.Background(), labels, "vm-2", "jit", "owner", "repo")
	if err == nil || !strings.Contains(err.Error(), "QUOTA_EXCEEDED") {
		t.Fatalf("expected a quota error to surface, got %v", err)
	}
}

// Consecutive jobs must not all pile onto whichever zone is listed first.
func TestConsecutiveJobsStartInDifferentZones(t *testing.T) {
	t.Setenv("GCRUNNER_ZONES", "zone-a,zone-b,zone-c")
	var first []string
	withInsert(t, func(_ context.Context, _, zone, _ string, _ *RunnerLabels, _, _ string) error {
		first = append(first, zone)
		return nil
	})

	labels := &RunnerLabels{Machine: "n2-standard-2", MachineMode: "exact"}
	for i := 0; i < 3; i++ {
		if err := createRunnerInstance(context.Background(), labels, "vm", "jit", "owner", "repo"); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if first[0] == first[1] && first[1] == first[2] {
		t.Fatalf("expected the starting zone to rotate, all three started on %s", first[0])
	}
}
