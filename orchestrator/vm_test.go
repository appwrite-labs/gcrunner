package function

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// startVM creates a VM for a job and boots it: the startup script handed to
// the zone that accepted the VM runs for real in a scratch runner home, with
// only the commands that need a GCE VM stubbed (metadata curl, sudo to the
// runner user, ownership changes). It returns that home so tests can read what
// the runner and docker will find there. fail marks zones that reject the VM.
func startVM(t *testing.T, zones string, fail map[string]error, home string) {
	t.Helper()
	t.Setenv("GCRUNNER_ZONES", zones)
	t.Setenv("GCE_REGION", "europe-west1")
	zoneOffset.Store(0)

	var script string
	withInsert(t, func(_ context.Context, _, zone, _ string, _ *RunnerLabels, startupScript, _ string) error {
		script = startupScript
		return fail[zone]
	})
	labels := &RunnerLabels{Machine: "n2-standard-2", MachineMode: "exact"}
	if err := createRunnerInstance(context.Background(), labels, "vm", "jit", "owner", "repo"); err != nil {
		t.Fatalf("createRunnerInstance: %v", err)
	}

	stubs := t.TempDir()
	for name, body := range map[string]string{
		"curl":    "echo jit",
		"sudo":    "exit 0",
		"chown":   "exit 0",
		"install": `mkdir -p "${!#}"`,
	} {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte("#!/bin/bash\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Docker configuration is exercised separately with isolated paths/stubs.
	// Never restart the test host's Docker service.
	if !strings.Contains(script, dockerNetworkScript) || strings.Index(script, dockerNetworkScript) > strings.Index(script, "sudo -u runner") {
		t.Fatal("Docker network setup must run before the Actions runner")
	}
	script = strings.Replace(script, dockerNetworkScript, "", 1)
	boot := exec.Command("bash", "-c", strings.ReplaceAll(script, "/home/runner", home))
	boot.Dir = home
	boot.Env = append(os.Environ(), "PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"))
	if out, err := boot.CombinedOutput(); err != nil {
		t.Fatalf("startup script failed: %v\n%s", err, out)
	}
}

// jobEnvironment is the extra environment the runner loads from .env into
// every job step.
func jobEnvironment(t *testing.T, home string) map[string]string {
	t.Helper()
	env := map[string]string{}
	raw, err := os.ReadFile(filepath.Join(home, ".env"))
	if os.IsNotExist(err) {
		return env
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			env[key] = value
		}
	}
	return env
}

// dockerConfig is what docker reads to decide how to authenticate to a host.
func dockerConfig(t *testing.T, home string) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, ".docker", "config.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatalf("docker config is not valid JSON (%v):\n%s", err, raw)
	}
	return config
}

func credentialHelpers(t *testing.T, home string) map[string]string {
	t.Helper()
	helpers := map[string]string{}
	if raw, ok := dockerConfig(t, home)["credHelpers"]; ok {
		if err := json.Unmarshal(raw, &helpers); err != nil {
			t.Fatal(err)
		}
	}
	return helpers
}

const (
	registry    = "europe-west1-docker.pkg.dev/p/gcrunner-registry"
	usEastCache = "us-east1-docker.pkg.dev/p/gcrunner-registry-us-east1"
	asiaCache   = "asia-east1-docker.pkg.dev/p/gcrunner-registry-asia-east1"
	caches      = "us-east1=" + usEastCache + ",asia-east1=" + asiaCache
)

func TestJobPushesToTheRegistryAndPullsFromTheCacheInItsOwnRegion(t *testing.T) {
	t.Setenv("GCRUNNER_REGISTRY", registry)
	t.Setenv("GCRUNNER_REGISTRY_PULLS", caches)
	home := t.TempDir()

	startVM(t, "us-east1-b", nil, home)

	env := jobEnvironment(t, home)
	if env["GCRUNNER_REGISTRY"] != registry || env["GCRUNNER_REGISTRY_PULL"] != usEastCache {
		t.Errorf("job sees %v, want the registry to push to and the us-east1 cache to pull from", env)
	}
	helpers := credentialHelpers(t, home)
	for _, host := range []string{"europe-west1-docker.pkg.dev", "us-east1-docker.pkg.dev"} {
		if helpers[host] != "gcloud" {
			t.Errorf("docker authenticates %v, want gcloud for %s", helpers, host)
		}
	}
}

// A VM in the deployment region has no cache of its own and pulls straight from
// the registry, and so does a zone= label that lands outside the pool.
func TestJobWithoutARegionalCachePullsFromTheRegistry(t *testing.T) {
	t.Setenv("GCRUNNER_REGISTRY", registry)
	t.Setenv("GCRUNNER_REGISTRY_PULLS", caches)

	for _, zone := range []string{"europe-west1-b", "southamerica-east1-a"} {
		home := t.TempDir()
		startVM(t, zone, nil, home)
		if env := jobEnvironment(t, home); env["GCRUNNER_REGISTRY_PULL"] != registry {
			t.Errorf("%s: job pulls from %q, want the registry", zone, env["GCRUNNER_REGISTRY_PULL"])
		}
	}
}

// The pull URL must follow the zone that accepted the VM, not the first one
// tried, or a job that fell through a quota failure would pull cross-region.
func TestJobPullsFromTheRegionOfTheZoneThatAcceptedIt(t *testing.T) {
	t.Setenv("GCRUNNER_REGISTRY", registry)
	t.Setenv("GCRUNNER_REGISTRY_PULLS", caches)
	home := t.TempDir()

	startVM(t, "us-east1-b,asia-east1-a", map[string]error{"us-east1-b": errQuota}, home)

	if env := jobEnvironment(t, home); env["GCRUNNER_REGISTRY_PULL"] != asiaCache {
		t.Errorf("job pulls from %q, want the asia-east1 cache after us-east1 ran out of quota", env["GCRUNNER_REGISTRY_PULL"])
	}
}

func TestJobWithoutCachesConfiguredPullsFromTheRegistry(t *testing.T) {
	t.Setenv("GCRUNNER_REGISTRY", registry)
	t.Setenv("GCRUNNER_REGISTRY_PULLS", "")
	home := t.TempDir()

	startVM(t, "us-east1-b", nil, home)

	if env := jobEnvironment(t, home); env["GCRUNNER_REGISTRY_PULL"] != registry {
		t.Errorf("job pulls from %q, want the registry when no caches exist", env["GCRUNNER_REGISTRY_PULL"])
	}
}

func TestJobGetsNoRegistryWhenItIsDisabled(t *testing.T) {
	t.Setenv("GCRUNNER_REGISTRY", "")
	t.Setenv("GCRUNNER_REGISTRY_PULLS", "")
	home := t.TempDir()

	startVM(t, "us-east1-b", nil, home)

	if env := jobEnvironment(t, home); len(env) != 0 {
		t.Errorf("job sees %v, want no registry variables", env)
	}
	if config := dockerConfig(t, home); config != nil {
		t.Errorf("docker config was written: %s", config)
	}
}

// An image may ship its own docker config, for example a login to a private
// mirror; the registry must be added to it rather than replace it.
func TestRegistryIsAddedToTheImagesDockerConfig(t *testing.T) {
	t.Setenv("GCRUNNER_REGISTRY", registry)
	t.Setenv("GCRUNNER_REGISTRY_PULLS", caches)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".docker"), 0o700); err != nil {
		t.Fatal(err)
	}
	existing := `{"auths":{"mirror.example.com":{"auth":"c2VjcmV0"}},"credHelpers":{"ghcr.io":"gh"}}`
	if err := os.WriteFile(filepath.Join(home, ".docker", "config.json"), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	startVM(t, "us-east1-b", nil, home)

	config := dockerConfig(t, home)
	if !strings.Contains(string(config["auths"]), "mirror.example.com") {
		t.Errorf("image's auths were lost: %s", config["auths"])
	}
	helpers := credentialHelpers(t, home)
	if helpers["ghcr.io"] != "gh" || helpers["us-east1-docker.pkg.dev"] != "gcloud" {
		t.Errorf("credential helpers %v, want the image's ghcr.io entry kept alongside the registry hosts", helpers)
	}
}
