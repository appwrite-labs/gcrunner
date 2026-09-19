package function

import (
	"context"
	"errors"
	"net/http"
	"strings"
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

func TestParseRepositoryConfig(t *testing.T) {
	config, err := parseRepositoryConfig([]byte(exampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Settings{
		"build": {labelFamily: "n2d+c3", labelCPU: "4+8", labelRAM: "16", labelDisk: "100gb", labelImage: "ci"},
		"e2e":   {labelFamily: "n2", labelCPU: "16", labelRAM: "16", labelDisk: "100gb", labelImage: "ci", labelSpot: "false"},
		"tiny":  {labelMachine: "e2-micro"},
	}
	for name, settings := range want {
		preset, ok := config.Runners[name]
		if !ok {
			t.Fatalf("runner %q missing", name)
		}
		got := preset.settings()
		if len(got) != len(settings) {
			t.Errorf("runner %q settings = %v, want %v", name, got, settings)
		}
		for key, value := range settings {
			if got[key] != value {
				t.Errorf("runner %q %s = %q, want %q", name, key, got[key], value)
			}
		}
	}
	if config.Images["ci"].Family != "ci-ubuntu2404-x64" || config.Images["ci"].Project != "my-images" {
		t.Errorf("image ci = %+v", config.Images["ci"])
	}
}

func TestParseRepositoryConfig_RejectsNestedList(t *testing.T) {
	_, err := parseRepositoryConfig([]byte("runners:\n  a:\n    family: [[n2]]\n"))
	if err == nil || !strings.Contains(err.Error(), "scalar") {
		t.Errorf("err = %v, want a scalar list item error", err)
	}
}

func TestResolve(t *testing.T) {
	config, err := parseRepositoryConfig([]byte(exampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		label string
		want  RunnerLabels
		err   string
	}{
		{
			name:  "preset over defaults",
			label: "gcrunner=1/runner=build",
			want: RunnerLabels{
				RunID: "1", Machine: "n2d-standard-2", Family: "n2d+c3", CPU: "4+8", RAM: "16", Spot: true,
				Disk: "100gb", DiskType: "pd-ssd", Image: "projects/my-images/global/images/family/ci-ubuntu2404-x64",
				MachineMode: machineModeFamily,
			},
		},
		{
			name:  "job labels over preset",
			label: "gcrunner=1/runner=e2e/disk=200gb/spot=true",
			want: RunnerLabels{
				RunID: "1", Machine: "n2d-standard-2", Family: "n2", CPU: "16", RAM: "16", Spot: true,
				Disk: "200gb", DiskType: "pd-ssd", Image: "projects/my-images/global/images/family/ci-ubuntu2404-x64",
				MachineMode: machineModeFamily,
			},
		},
		{
			name:  "job machine= is exact despite the preset's family",
			label: "gcrunner=1/runner=build/machine=c3-standard-8",
			want: RunnerLabels{
				RunID: "1", Machine: "c3-standard-8", CPU: "4+8", RAM: "16", Spot: true,
				Disk: "100gb", DiskType: "pd-ssd", Image: "projects/my-images/global/images/family/ci-ubuntu2404-x64",
				MachineMode: machineModeExact,
			},
		},
		{
			name:  "job cpu= keeps the preset's family",
			label: "gcrunner=1/runner=build/cpu=16",
			want: RunnerLabels{
				RunID: "1", Machine: "n2d-standard-2", Family: "n2d+c3", CPU: "16", RAM: "16", Spot: true,
				Disk: "100gb", DiskType: "pd-ssd", Image: "projects/my-images/global/images/family/ci-ubuntu2404-x64",
				MachineMode: machineModeFamily,
			},
		},
		{
			name:  "preset without cpu or family stays exact",
			label: "gcrunner=1/runner=tiny",
			want: RunnerLabels{
				RunID: "1", Machine: "e2-micro", Spot: true, Disk: "75gb", DiskType: "pd-ssd", Image: "ubuntu24-full-x64",
				MachineMode: machineModeExact,
			},
		},
		{
			name:  "image by name in the default project",
			label: "gcrunner=1/image=pinned",
			want: RunnerLabels{
				RunID: "1", Machine: "n2d-standard-2", Spot: true, Disk: "75gb", DiskType: "pd-ssd",
				Image: "projects/default-images/global/images/ci-ubuntu2404-x64-20260917", MachineMode: machineModeExact,
			},
		},
		{
			name:  "image not in the file passes through",
			label: "gcrunner=1/image=ubuntu22-full-x64",
			want: RunnerLabels{
				RunID: "1", Machine: "n2d-standard-2", Spot: true, Disk: "75gb", DiskType: "pd-ssd",
				Image: "ubuntu22-full-x64", MachineMode: machineModeExact,
			},
		},
		{
			name:  "unknown runner",
			label: "gcrunner=1/runner=nope",
			err:   `runner "nope" is not defined`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := config.resolve(parseJobLabels([]string{tt.label}), "default-images")
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if *got != tt.want {
				t.Errorf("resolve() =\n%+v\nwant\n%+v", *got, tt.want)
			}
		})
	}
}

func TestResolve_NoConfigFile(t *testing.T) {
	config := &RepositoryConfig{}
	got, err := config.resolve(parseJobLabels([]string{"gcrunner=1/cpu=4"}), "p")
	if err != nil {
		t.Fatal(err)
	}
	if got.MachineMode != machineModeAuto || got.CPU != "4" {
		t.Errorf("resolve() = %+v", got)
	}
	if _, err := config.resolve(parseJobLabels([]string{"gcrunner=1/runner=build"}), "p"); err == nil {
		t.Error("runner= without a config file should be an error")
	}
}

func TestImageDefinitionPath(t *testing.T) {
	tests := []struct {
		definition ImageDefinition
		want       string
		err        bool
	}{
		{ImageDefinition{Family: "f"}, "projects/p/global/images/family/f", false},
		{ImageDefinition{Project: "q", Name: "n"}, "projects/q/global/images/n", false},
		{ImageDefinition{Family: "f", Name: "n"}, "", true},
		{ImageDefinition{}, "", true},
	}
	for _, tt := range tests {
		got, err := tt.definition.path("p")
		if (err != nil) != tt.err || got != tt.want {
			t.Errorf("path(%+v) = %q, %v; want %q, err=%v", tt.definition, got, err, tt.want, tt.err)
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
	var calls int
	var reply []byte
	var replyErr error
	fetchRepositoryFile = func(_ context.Context, owner, repo, path, ref string) ([]byte, error) {
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
		t.Errorf("a commit was fetched %d times, want once", calls)
	}

	for i := 0; i < 2; i++ {
		if _, err := cache.load(context.Background(), "o", "r", "main", false); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 3 {
		t.Errorf("a branch was fetched %d times in total, want every time", calls)
	}

	now = now.Add(2 * time.Hour)
	if _, err := cache.load(context.Background(), "o", "r", sha, true); err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Errorf("an expired commit entry was not refetched (calls=%d)", calls)
	}

	reply, replyErr = nil, nil
	config, err := cache.load(context.Background(), "o", "r", "main", false)
	if err != nil || len(config.Runners) != 0 {
		t.Errorf("missing file: config=%+v err=%v, want empty and nil", config, err)
	}

	replyErr = &githubError{Status: http.StatusForbidden, Body: "Resource not accessible by integration"}
	config, err = cache.load(context.Background(), "o", "r", "main", false)
	if err != nil || len(config.Runners) != 0 {
		t.Errorf("forbidden: config=%+v err=%v, want empty and nil", config, err)
	}

	replyErr = errors.New("connection reset")
	if _, err = cache.load(context.Background(), "o", "r", "main", false); err == nil {
		t.Error("a transport error should be returned so the task retries")
	}

	reply, replyErr = []byte("runners: [not a map]"), nil
	if _, err = cache.load(context.Background(), "o", "r", "main", false); err == nil {
		t.Error("a malformed file should be an error")
	}
}
