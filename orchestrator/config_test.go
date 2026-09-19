package function

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

const exampleConfig = `
images:
  ci:
    project: my-images
    family: ci-ubuntu2404-x64
  pinned:
    name: ci-ubuntu2404-x64-20260917

runners:
  build: &build
    family: [n2d, c3]
    cpu: [4, 8]
    ram: 16
    disk: 100gb
    image: ci
  e2e:
    <<: *build
    family: n2
    cpu: 16
    spot: false
  tiny:
    machine: e2-micro
`

// resolveWith runs a label through the file and returns the machine the job
// would get, as the zone loop sees it: the resolver's answer for an exact
// request, or the family and constraints it would search with.
func resolveWith(t *testing.T, config *RepositoryConfig, label string) *RunnerLabels {
	t.Helper()
	runner, err := config.resolve(parseJobLabels([]string{label}), "default-images")
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func TestResolve(t *testing.T) {
	config, err := parseRepositoryConfig([]byte(exampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	const ciImage = "projects/my-images/global/images/family/ci-ubuntu2404-x64"

	t.Run("preset fills in what the job did not say", func(t *testing.T) {
		runner := resolveWith(t, config, "gcrunner=1/runner=build")
		if runner.Family != "n2d+c3" || runner.CPU != "4+8" || runner.RAM != "16" {
			t.Errorf("machine search = family %s cpu %s ram %s, want n2d+c3 4+8 16 from the preset", runner.Family, runner.CPU, runner.RAM)
		}
		if runner.Disk != "100gb" || !runner.Spot || runner.Image != ciImage {
			t.Errorf("disk %s spot %v image %s: want the preset's disk, the default spot, and the configured image", runner.Disk, runner.Spot, runner.Image)
		}
	})

	t.Run("yaml merge keys and lists work", func(t *testing.T) {
		runner := resolveWith(t, config, "gcrunner=1/runner=e2e")
		if runner.Family != "n2" || runner.CPU != "16" || runner.RAM != "16" || runner.Spot {
			t.Errorf("e2e = %+v, want n2 with 16 vCPUs, 16 GB inherited from build, on demand", runner)
		}
	})

	t.Run("job labels win over the preset", func(t *testing.T) {
		runner := resolveWith(t, config, "gcrunner=1/runner=e2e/disk=200gb/spot=true")
		if runner.Disk != "200gb" || !runner.Spot || runner.Family != "n2" {
			t.Errorf("runner = %+v, want the job's disk and spot on the e2e shape", runner)
		}
	})

	t.Run("a job machine= is exact despite the preset family", func(t *testing.T) {
		runner := resolveWith(t, config, "gcrunner=1/runner=build/machine=c3-standard-8")
		got, err := ResolveMachineType(context.Background(), "project", "zone", runner)
		if err != nil || got != "c3-standard-8" {
			t.Errorf("resolved %q, %v; want the machine the job asked for without a lookup", got, err)
		}
	})

	t.Run("a job cpu= keeps the preset family", func(t *testing.T) {
		runner := resolveWith(t, config, "gcrunner=1/runner=build/cpu=16")
		if runner.Family != "n2d+c3" || runner.CPU != "16" {
			t.Errorf("machine search = family %s cpu %s, want the preset's families with 16 vCPUs", runner.Family, runner.CPU)
		}
	})

	t.Run("a job cpu= replaces the preset's pinned machine", func(t *testing.T) {
		runner := resolveWith(t, config, "gcrunner=1/runner=tiny/cpu=8")
		if runner.Machine == "e2-micro" || runner.CPU != "8" || runner.MachineMode == machineModeExact {
			t.Errorf("runner = %+v, want a machine resolved for 8 vCPUs, not e2-micro", runner)
		}
	})

	t.Run("an exact preset needs no lookup", func(t *testing.T) {
		runner := resolveWith(t, config, "gcrunner=1/runner=tiny")
		got, err := ResolveMachineType(context.Background(), "project", "zone", runner)
		if err != nil || got != "e2-micro" {
			t.Errorf("resolved %q, %v; want e2-micro", got, err)
		}
	})

	t.Run("images resolve by name in the default project", func(t *testing.T) {
		runner := resolveWith(t, config, "gcrunner=1/image=pinned")
		if want := "projects/default-images/global/images/ci-ubuntu2404-x64-20260917"; resolveSourceImage(runner.Image) != want {
			t.Errorf("source image = %q, want %q", resolveSourceImage(runner.Image), want)
		}
	})

	t.Run("an image the file does not name is left to the built-ins", func(t *testing.T) {
		runner := resolveWith(t, config, "gcrunner=1/image=ubuntu22-full-x64")
		if resolveSourceImage(runner.Image) != resolveSourceImage("ubuntu22-full-x64") {
			t.Errorf("image %q was rewritten", runner.Image)
		}
	})

	t.Run("unknown runner is held, not retried", func(t *testing.T) {
		_, err := config.resolve(parseJobLabels([]string{"gcrunner=1/runner=nope"}), "p")
		if !errors.Is(err, errConfiguration) || !strings.Contains(err.Error(), `"nope"`) {
			t.Errorf("err = %v, want a configuration error naming the runner", err)
		}
	})
}

func TestResolve_WithoutFile(t *testing.T) {
	config := &RepositoryConfig{}
	runner := resolveWith(t, config, "gcrunner=1/cpu=4")
	if runner.CPU != "4" || runner.Family != "" {
		t.Errorf("runner = %+v, want the label alone", runner)
	}
	if _, err := config.resolve(parseJobLabels([]string{"gcrunner=1/runner=build"}), "p"); !errors.Is(err, errConfiguration) {
		t.Errorf("runner= without a file: err = %v, want a configuration error", err)
	}
}

func TestResolve_UnreadableFileHoldsPresetJobs(t *testing.T) {
	config := &RepositoryConfig{unreadable: true}
	if _, err := config.resolve(parseJobLabels([]string{"gcrunner=1/runner=build"}), "p"); !errors.Is(err, errConfiguration) || !strings.Contains(err.Error(), "read access") {
		t.Errorf("err = %v, want a configuration error naming the missing permission", err)
	}
	if runner := resolveWith(t, config, "gcrunner=1/cpu=4"); runner.CPU != "4" {
		t.Errorf("a label-only job = %+v, want it to run as before", runner)
	}
}

func TestParseRepositoryConfig_Rejects(t *testing.T) {
	tests := map[string]string{
		"unknown key":      "runners:\n  a:\n    familly: n2\n",
		"nested list":      "runners:\n  a:\n    family: [[n2]]\n",
		"cpu word":         "runners:\n  a:\n    cpu: lots\n",
		"ram three values": "runners:\n  a:\n    ram: [4, 8, 16]\n",
		"disk unit":        "runners:\n  a:\n    disk: 100tb\n",
		"runners list":     "runners: [not a map]",
	}
	for name, file := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := parseRepositoryConfig([]byte(file))
			if !errors.Is(err, errConfiguration) {
				t.Errorf("err = %v, want a configuration error", err)
			}
		})
	}
	if config, err := parseRepositoryConfig(nil); err != nil || len(config.Runners) != 0 {
		t.Errorf("empty file: %+v, %v; want an empty config", config, err)
	}
}

func TestImageDefinition_RejectsAmbiguity(t *testing.T) {
	for _, definition := range []ImageDefinition{{Family: "f", Name: "n"}, {}} {
		if _, err := definition.path("p"); err == nil {
			t.Errorf("path(%+v) succeeded, want an error", definition)
		}
	}
}

func TestConfigRef(t *testing.T) {
	event := WorkflowJobEvent{
		WorkflowJob: WorkflowJob{HeadSHA: "abc"},
		Repository:  Repository{Private: true, DefaultBranch: "main"},
	}
	if got := configRef(event); got != "abc" {
		t.Errorf("private repo ref = %q, want the job's commit", got)
	}
	event.Repository.Private = false
	if got := configRef(event); got != "main" {
		t.Errorf("public repo ref = %q, want the default branch", got)
	}
}

func TestConfigCacheLoad(t *testing.T) {
	sha := strings.Repeat("a", 40)
	var mu sync.Mutex
	var calls int
	var reply []byte
	var replyErr error
	fetchRepositoryFile = func(_ context.Context, owner, repo, path, ref string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if path != configPath {
			t.Errorf("path = %q", path)
		}
		return reply, replyErr
	}
	t.Cleanup(func() { fetchRepositoryFile = fetchRepositoryContents })

	now := time.Now()
	cache := &ConfigCache{configs: map[string]configCacheEntry{}, ttl: time.Hour, nowFunc: func() time.Time { return now }}

	reply = []byte("runners:\n  a:\n    cpu: 4\n")
	for i := 0; i < 2; i++ {
		config, err := cache.load(context.Background(), "o", "r", sha, true)
		if err != nil {
			t.Fatal(err)
		}
		if config.Runners["a"].CPU != "4" {
			t.Errorf("runner a = %+v", config.Runners["a"])
		}
	}
	if calls != 1 {
		t.Errorf("a commit was read from GitHub %d times, want once", calls)
	}

	reply = []byte("runners:\n  a:\n    cpu: 8\n")
	config, _ := cache.load(context.Background(), "o", "r", "main", false)
	if config.Runners["a"].CPU != "8" {
		t.Errorf("a branch edit was not seen on the next job: cpu = %q", config.Runners["a"].CPU)
	}

	now = now.Add(2 * time.Hour)
	config, _ = cache.load(context.Background(), "o", "r", sha, true)
	if config.Runners["a"].CPU != "8" {
		t.Errorf("an expired commit entry was still served: cpu = %q", config.Runners["a"].CPU)
	}

	reply, replyErr = nil, nil
	config, err := cache.load(context.Background(), "o", "r", "main", false)
	if err != nil || len(config.Runners) != 0 {
		t.Errorf("missing file: config=%+v err=%v, want empty and nil", config, err)
	}

	replyErr = &githubError{Status: http.StatusForbidden, Body: "Resource not accessible by integration"}
	other := strings.Repeat("b", 40)
	if _, err := cache.load(context.Background(), "o", "r", other, true); !errors.Is(err, errConfigForbidden) {
		t.Errorf("forbidden: err = %v, want the permission error", err)
	}
	reply, replyErr = []byte("runners:\n  a:\n    cpu: 4\n"), nil
	if config, err := cache.load(context.Background(), "o", "r", other, true); err != nil || config.Runners["a"].CPU != "4" {
		t.Errorf("after the permission is granted: config=%+v err=%v, want the file on the next job", config, err)
	}

	replyErr = errors.New("connection reset")
	if _, err := cache.load(context.Background(), "o", "r", "main", false); err == nil {
		t.Error("a transport error should be returned so the task retries")
	}

	reply, replyErr = []byte("runners: [not a map]"), nil
	if _, err := cache.load(context.Background(), "o", "r", "main", false); !errors.Is(err, errConfiguration) {
		t.Errorf("a malformed file: err = %v, want a configuration error no retry is attempted for", err)
	}
}

func TestConfigCacheLoad_CoalescesConcurrentMisses(t *testing.T) {
	var mu sync.Mutex
	var calls int
	entered := make(chan struct{})
	release := make(chan struct{})
	fetchRepositoryFile = func(ctx context.Context, _, _, _, _ string) ([]byte, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		close(entered)
		<-release
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return []byte("runners:\n  a:\n    cpu: 4\n"), nil
	}
	t.Cleanup(func() { fetchRepositoryFile = fetchRepositoryContents })

	cache := &ConfigCache{configs: map[string]configCacheEntry{}, ttl: time.Hour, nowFunc: time.Now}
	sha := strings.Repeat("c", 40)
	var wg sync.WaitGroup
	load := func(ctx context.Context) {
		defer wg.Done()
		config, err := cache.load(ctx, "o", "r", sha, true)
		if err != nil || config.Runners["a"].CPU != "4" {
			t.Errorf("config=%+v err=%v", config, err)
		}
	}

	// The first webhook to arrive starts the read and then gives up. The
	// jobs that arrived alongside it must still get the file.
	first, cancel := context.WithCancel(context.Background())
	wg.Add(1)
	go load(first)
	<-entered
	cancel()
	for i := 0; i < 19; i++ {
		wg.Add(1)
		go load(context.Background())
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls != 1 {
		t.Errorf("twenty jobs on one commit read GitHub %d times, want once", calls)
	}
}
