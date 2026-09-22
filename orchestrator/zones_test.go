package function

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	computepb "cloud.google.com/go/compute/apiv1/computepb"
)

var (
	errQuota    = errors.New("QUOTA_EXCEEDED: cpus")
	errStockout = errors.New("ZONE_RESOURCE_POOL_EXHAUSTED")
	errExists   = errors.New("instance already exists")
	errFatal    = errors.New("Permission denied")
)

func TestZoneSelection(t *testing.T) {
	tests := []struct {
		name       string
		label      string           // zone= label, "+"-separated
		pool       string           // GCRUNNER_ZONES
		discovered []string         // zones seeded into the region cache
		fail       map[string]error // per-zone insert failure; missing = success
		jobs       int              // consecutive jobs, default 1
		wantTried  [][]string       // zones attempted, per job
		wantErr    string           // substring of the final error, "" for success
	}{
		{
			name:      "label zones are tried in the order given",
			label:     "zone-b+zone-a",
			jobs:      3,
			wantTried: [][]string{{"zone-b"}, {"zone-b"}, {"zone-b"}},
		},
		{
			name:      "pool zones rotate across consecutive jobs",
			pool:      "zone-a,zone-b,zone-c",
			jobs:      3,
			wantTried: [][]string{{"zone-a"}, {"zone-b"}, {"zone-c"}},
		},
		{
			name:       "discovered zones rotate across consecutive jobs",
			discovered: []string{"zone-a", "zone-b"},
			jobs:       2,
			wantTried:  [][]string{{"zone-a"}, {"zone-b"}},
		},
		{
			name:      "label beats pool",
			label:     "zone-x",
			pool:      "zone-a,zone-b",
			wantTried: [][]string{{"zone-x"}},
		},
		{
			name:      "quota in one zone falls through to the next",
			label:     "zone-a+zone-b",
			fail:      map[string]error{"zone-a": errQuota},
			wantTried: [][]string{{"zone-a", "zone-b"}},
		},
		{
			name:      "stockout in one zone falls through to the next",
			label:     "zone-a+zone-b",
			fail:      map[string]error{"zone-a": errStockout},
			wantTried: [][]string{{"zone-a", "zone-b"}},
		},
		{
			name:      "rotation wraps so every pool zone is still tried",
			pool:      "zone-a,zone-b,zone-c",
			fail:      map[string]error{"zone-b": errQuota, "zone-c": errQuota},
			jobs:      2,
			wantTried: [][]string{{"zone-a"}, {"zone-b", "zone-c", "zone-a"}},
		},
		{
			name:      "quota everywhere is reported as quota",
			label:     "zone-a+zone-b",
			fail:      map[string]error{"zone-a": errQuota, "zone-b": errQuota},
			wantTried: [][]string{{"zone-a", "zone-b"}},
			wantErr:   "every zone out of quota",
		},
		{
			name:      "quota plus stockout names the quota zone and keeps the stockout",
			label:     "zone-a+zone-b",
			fail:      map[string]error{"zone-a": errQuota, "zone-b": errStockout},
			wantTried: [][]string{{"zone-a", "zone-b"}},
			wantErr:   "out of quota in zone-a): failed to create VM in zone-b: ZONE_RESOURCE_POOL_EXHAUSTED",
		},
		{
			name:      "stockout everywhere is not a quota error",
			label:     "zone-a+zone-b",
			fail:      map[string]error{"zone-a": errStockout, "zone-b": errStockout},
			wantTried: [][]string{{"zone-a", "zone-b"}},
			wantErr:   "failed to create VM in any zone: failed to create VM in zone-b",
		},
		{
			name:      "duplicate webhook is a success and stops",
			label:     "zone-a+zone-b",
			fail:      map[string]error{"zone-a": errExists},
			wantTried: [][]string{{"zone-a"}},
		},
		{
			name:      "fatal error stops without trying the next zone",
			label:     "zone-a+zone-b",
			fail:      map[string]error{"zone-a": errFatal},
			wantTried: [][]string{{"zone-a"}},
			wantErr:   "Permission denied",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GCRUNNER_ZONES", tt.pool)
			t.Setenv("GCE_REGION", "test-region")
			seedZones(t, "test-region", tt.discovered)
			zoneOffset.Store(0)

			var tried []string
			withInsert(t, func(_ context.Context, zone string, _ *computepb.Instance) error {
				tried = append(tried, zone)
				return tt.fail[zone]
			})

			labels := &RunnerLabels{Zone: tt.label, Machine: "n2-standard-2", MachineMode: "exact"}
			jobs := max(tt.jobs, 1)
			var err error
			var got [][]string
			for i := 0; i < jobs; i++ {
				tried = nil
				err = createRunnerInstance(context.Background(), labels, fmt.Sprintf("vm-%d", i), "jit", "owner", "repo")
				got = append(got, tried)
			}

			if !reflect.DeepEqual(got, tt.wantTried) {
				t.Errorf("tried %v, want %v", got, tt.wantTried)
			}
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("error %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestRotate(t *testing.T) {
	tests := []struct {
		zones  []string
		offset int
		want   []string
	}{
		{nil, 1, nil},
		{[]string{"a"}, 1, []string{"a"}},
		{[]string{"a", "b", "c"}, 0, []string{"a", "b", "c"}},
		{[]string{"a", "b", "c"}, 1, []string{"b", "c", "a"}},
		{[]string{"a", "b", "c"}, 4, []string{"b", "c", "a"}},
	}
	for _, tt := range tests {
		if got := rotate(tt.zones, tt.offset); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("rotate(%v, %d) = %v, want %v", tt.zones, tt.offset, got, tt.want)
		}
	}
}

func TestConfiguredZones(t *testing.T) {
	tests := []struct {
		env  string
		want []string
	}{
		{"", nil},
		{" , ", nil},
		{"us-central1-a", []string{"us-central1-a"}},
		{"us-central1-a, europe-west1-b ,", []string{"us-central1-a", "europe-west1-b"}},
	}
	for _, tt := range tests {
		t.Setenv("GCRUNNER_ZONES", tt.env)
		if got := configuredZones(); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("GCRUNNER_ZONES=%q: got %v, want %v", tt.env, got, tt.want)
		}
	}
}

func withInsert(t *testing.T, fn func(ctx context.Context, zone string, instance *computepb.Instance) error) {
	t.Helper()
	original := insertInstance
	insertInstance = fn
	t.Cleanup(func() { insertInstance = original })
}

func seedZones(t *testing.T, region string, zones []string) {
	t.Helper()
	if zones == nil {
		return
	}
	zoneCache.mu.Lock()
	zoneCache.zones[region] = zoneCacheEntry{zones: zones, fetchedAt: time.Now()}
	zoneCache.mu.Unlock()
	t.Cleanup(func() {
		zoneCache.mu.Lock()
		delete(zoneCache.zones, region)
		zoneCache.mu.Unlock()
	})
}

func TestZoneCache_TTL(t *testing.T) {
	now := time.Now()
	cache := &ZoneCache{
		zones:   make(map[string]zoneCacheEntry),
		ttl:     1 * time.Hour,
		nowFunc: func() time.Time { return now },
	}

	// Manually populate cache
	cache.zones["us-central1"] = zoneCacheEntry{
		zones:     []string{"us-central1-a", "us-central1-b", "us-central1-c"},
		fetchedAt: now,
	}

	// Should be valid immediately
	cache.mu.RLock()
	entry, ok := cache.zones["us-central1"]
	cache.mu.RUnlock()
	if !ok {
		t.Fatal("expected cache entry")
	}
	if now.Sub(entry.fetchedAt) >= cache.ttl {
		t.Error("cache entry should be valid")
	}

	// After TTL expires, entry should be stale
	cache.nowFunc = func() time.Time { return now.Add(2 * time.Hour) }
	staleNow := cache.nowFunc()
	if staleNow.Sub(entry.fetchedAt) < cache.ttl {
		t.Error("cache entry should be stale after TTL")
	}
}

func TestZoneCache_DifferentRegions(t *testing.T) {
	now := time.Now()
	cache := &ZoneCache{
		zones:   make(map[string]zoneCacheEntry),
		ttl:     1 * time.Hour,
		nowFunc: func() time.Time { return now },
	}

	cache.zones["us-central1"] = zoneCacheEntry{
		zones:     []string{"us-central1-a", "us-central1-b"},
		fetchedAt: now,
	}
	cache.zones["europe-west1"] = zoneCacheEntry{
		zones:     []string{"europe-west1-b", "europe-west1-c", "europe-west1-d"},
		fetchedAt: now,
	}

	if len(cache.zones["us-central1"].zones) != 2 {
		t.Errorf("us-central1 zones = %d, want 2", len(cache.zones["us-central1"].zones))
	}
	if len(cache.zones["europe-west1"].zones) != 3 {
		t.Errorf("europe-west1 zones = %d, want 3", len(cache.zones["europe-west1"].zones))
	}
}

func TestClassifyInsertError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want insertErrorKind
	}{
		{"nil error", nil, insertErrorRetryable},
		{"quota exceeded", fmt.Errorf("QUOTA_EXCEEDED: insufficient regional quota"), insertErrorQuota},
		{"not found", fmt.Errorf("RESOURCE_NOT_FOUND: machine type not available"), insertErrorFatal},
		{"permission denied", fmt.Errorf("Permission denied on resource"), insertErrorFatal},
		{"already exists", fmt.Errorf("The resource 'projects/foo/zones/us-central1-a/instances/gcrunner-123' already exists"), insertErrorAlreadyExists},
		{"already exists camel", fmt.Errorf("alreadyExists"), insertErrorAlreadyExists},
		{"zone exhausted", fmt.Errorf("ZONE_RESOURCE_POOL_EXHAUSTED"), insertErrorRetryable},
		{"deleted image", fmt.Errorf(deletedImageError), insertErrorPermanent},
		{"machine type missing in zone", fmt.Errorf(missingMachineTypeError), insertErrorNoMachineType},
		{"generic error", fmt.Errorf("some transient error"), insertErrorRetryable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyInsertError(tt.err)
			if got != tt.want {
				t.Errorf("classifyInsertError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
