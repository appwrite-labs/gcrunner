package function

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"
)

// registryFor creates a VM for a job and reports the registry configuration
// the startup script hands the job: the push URL, the pull URL and the hosts
// docker will mint tokens for. fail marks zones that reject the VM.
func registryFor(t *testing.T, zones string, fail map[string]error) (push, pull string, hosts []string) {
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

	urls := regexp.MustCompile(`GCRUNNER_REGISTRY=%s\\nGCRUNNER_REGISTRY_PULL=%s\\n' "([^"]*)" "([^"]*)"`).FindStringSubmatch(script)
	if urls == nil {
		return "", "", nil
	}
	config := regexp.MustCompile(`'(\{"credHelpers":.*?\})'`).FindStringSubmatch(script)
	var docker struct {
		CredHelpers map[string]string `json:"credHelpers"`
	}
	if config == nil || json.Unmarshal([]byte(config[1]), &docker) != nil {
		t.Fatalf("startup script has no valid docker config:\n%s", script)
	}
	for host, helper := range docker.CredHelpers {
		if helper != "gcloud" {
			t.Errorf("%s uses helper %q, want gcloud", host, helper)
		}
		hosts = append(hosts, host)
	}
	return urls[1], urls[2], hosts
}

const (
	registry    = "europe-west1-docker.pkg.dev/p/gcrunner-registry"
	usEastCache = "us-east1-docker.pkg.dev/p/gcrunner-registry-us-east1"
	asiaCache   = "asia-east1-docker.pkg.dev/p/gcrunner-registry-asia-east1"
	caches      = "us-east1=" + usEastCache + ",asia-east1=" + asiaCache
)

func TestJobPullsFromTheCacheInItsOwnRegion(t *testing.T) {
	t.Setenv("GCRUNNER_REGISTRY", registry)
	t.Setenv("GCRUNNER_REGISTRY_PULLS", caches)

	push, pull, hosts := registryFor(t, "us-east1-b", nil)
	if push != registry || pull != usEastCache {
		t.Errorf("job got push=%q pull=%q, want the registry and the us-east1 cache", push, pull)
	}
	if len(hosts) != 2 {
		t.Errorf("docker authenticates %v, want both the registry and cache hosts", hosts)
	}
}

// A VM in the deployment region has no cache of its own and pulls straight from
// the registry, and so does a zone= label that lands outside the pool.
func TestJobWithoutARegionalCachePullsFromTheRegistry(t *testing.T) {
	t.Setenv("GCRUNNER_REGISTRY", registry)
	t.Setenv("GCRUNNER_REGISTRY_PULLS", caches)

	for _, zone := range []string{"europe-west1-b", "southamerica-east1-a"} {
		push, pull, hosts := registryFor(t, zone, nil)
		if push != registry || pull != registry {
			t.Errorf("%s: job got push=%q pull=%q, want the registry for both", zone, push, pull)
		}
		if len(hosts) != 1 {
			t.Errorf("%s: docker authenticates %v, want just the registry host", zone, hosts)
		}
	}
}

// The pull URL must follow the zone that accepted the VM, not the first one
// tried, or a job that fell through a quota failure would pull cross-region.
func TestPullRegistryFollowsTheZoneThatAcceptedTheVM(t *testing.T) {
	t.Setenv("GCRUNNER_REGISTRY", registry)
	t.Setenv("GCRUNNER_REGISTRY_PULLS", caches)

	_, pull, _ := registryFor(t, "us-east1-b,asia-east1-a", map[string]error{"us-east1-b": errQuota})
	if pull != asiaCache {
		t.Errorf("job pulls from %q, want the asia-east1 cache after us-east1 ran out of quota", pull)
	}
}

func TestJobGetsNoRegistryWhenItIsDisabled(t *testing.T) {
	t.Setenv("GCRUNNER_REGISTRY", "")
	t.Setenv("GCRUNNER_REGISTRY_PULLS", "")

	push, pull, hosts := registryFor(t, "us-east1-b", nil)
	if push != "" || pull != "" || hosts != nil {
		t.Errorf("job got push=%q pull=%q hosts=%v, want no registry configuration", push, pull, hosts)
	}
}

func TestJobWithoutCachesConfiguredPullsFromTheRegistry(t *testing.T) {
	t.Setenv("GCRUNNER_REGISTRY", registry)
	t.Setenv("GCRUNNER_REGISTRY_PULLS", "")

	if _, pull, _ := registryFor(t, "us-east1-b", nil); pull != registry {
		t.Errorf("job pulls from %q, want the registry when no caches exist", pull)
	}
}
