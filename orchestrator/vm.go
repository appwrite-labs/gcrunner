package function

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path"
	"strings"
	"sync/atomic"

	compute "cloud.google.com/go/compute/apiv1"
	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"
)

const startupScriptTemplate = `#!/bin/bash
set -euo pipefail

METADATA_URL="http://metadata.google.internal/computeMetadata/v1"
METADATA_HEADER="Metadata-Flavor: Google"

# Retrieve JIT config from instance metadata and delete it immediately
JIT_CONFIG=$(curl -sf -H "${METADATA_HEADER}" "${METADATA_URL}/instance/attributes/jit-config")
# Remove the metadata key so credentials are no longer queryable
curl -sf -X DELETE -H "${METADATA_HEADER}" \
  "${METADATA_URL}/instance/attributes/jit-config" || true

CACHE_BUCKET="%s"
REPO_OWNER="%s"
REPO_NAME="%s"

cd /home/runner

# Start cache server if bucket is configured
if [ -n "${CACHE_BUCKET}" ] && [ -x /usr/local/bin/cache-server ]; then
  /usr/local/bin/cache-server \
    -bucket "${CACHE_BUCKET}" \
    -owner "${REPO_OWNER}" \
    -repo "${REPO_NAME}" &
  for i in $(seq 1 10); do
    curl -sf http://localhost:8787/health && break
    sleep 0.5
  done
  export ACTIONS_RESULTS_URL="http://localhost:8787/"
  export ACTIONS_CACHE_SERVICE_V2=true
fi
%s
# Source /etc/environment for image-configured variables (HOME, NVM_DIR,
# XDG_CONFIG_HOME, PATH entries for cargo/pip, AGENT_TOOLSDIRECTORY, etc.)
# sudo does not go through PAM login, so these are not loaded automatically.
export HOME=/home/runner
if [ -f /etc/environment ]; then
  set -a
  . /etc/environment
  set +a
fi

# Run with JIT config (skips config.sh entirely)
sudo -u runner -E ./run.sh --jitconfig "${JIT_CONFIG}"
`

// registryScriptTemplate hands every job step the registry URLs through the
// runner's .env file and lets docker mint tokens for both hosts from the
// metadata server, so a long job never outlives a login.
const registryScriptTemplate = `
install -d -o runner -g runner -m 700 /home/runner/.docker
DOCKER_CONFIG_JSON='%s'
if [ -s /home/runner/.docker/config.json ] && command -v jq >/dev/null; then
  DOCKER_CONFIG_JSON=$(jq -c --argjson add "${DOCKER_CONFIG_JSON}" '. * $add' /home/runner/.docker/config.json)
fi
echo "${DOCKER_CONFIG_JSON}" > /home/runner/.docker/config.json
printf 'GCRUNNER_REGISTRY=%%s\nGCRUNNER_REGISTRY_PULL=%%s\n' "%s" "%s" >> /home/runner/.env
chown runner:runner /home/runner/.docker/config.json /home/runner/.env
`

func registryScript(zone string) string {
	registry := os.Getenv("GCRUNNER_REGISTRY")
	if registry == "" {
		return ""
	}
	pull := pullRegistry(registry, zone)
	helpers := map[string]string{registryHost(registry): "gcloud", registryHost(pull): "gcloud"}
	config, _ := json.Marshal(map[string]any{"credHelpers": helpers})
	return fmt.Sprintf(registryScriptTemplate, config, registry, pull)
}

// pullRegistry is the read-through cache for the VM's region, or the registry
// itself when that region has none. GCRUNNER_REGISTRY_PULLS is a comma-separated
// list of region=url pairs.
func pullRegistry(registry, zone string) string {
	region := zone[:max(strings.LastIndex(zone, "-"), 0)]
	for _, pair := range strings.Split(os.Getenv("GCRUNNER_REGISTRY_PULLS"), ",") {
		if cached, url, ok := strings.Cut(pair, "="); ok && cached == region {
			return url
		}
	}
	return registry
}

func registryHost(registry string) string {
	host, _, _ := strings.Cut(registry, "/")
	return host
}

func createRunnerVM(ctx context.Context, event WorkflowJobEvent, labels *RunnerLabels) error {
	owner := event.Repository.Owner.Login
	repo := event.Repository.Name

	instanceName := fmt.Sprintf("gcrunner-%d-%d", event.WorkflowJob.RunID, event.WorkflowJob.ID)

	// An earlier attempt can have created the VM and still returned an error.
	// That VM is booting with the JIT config from that attempt, so leave both
	// it and its registration alone.
	zone, err := findInstanceZone(ctx, instanceName)
	if err != nil {
		return err
	}
	if zone != "" {
		log.Printf("VM %s already exists in %s, leaving the earlier attempt to run the job", instanceName, zone)
		return nil
	}

	// No VM, so any registration left behind belongs to an attempt that never
	// reached one. GitHub refuses a second registration under the same name.
	if err := removeIdleRunner(ctx, owner, repo, instanceName); err != nil {
		return fmt.Errorf("remove stale runner %s: %w", instanceName, err)
	}

	// Generate JIT config (replaces registration token + config.sh)
	jitConfig, err := generateJITConfig(ctx, owner, repo, instanceName, event.WorkflowJob.Labels)
	if err != nil {
		return fmt.Errorf("generate JIT config: %w", err)
	}
	createErr := createRunnerInstance(ctx, labels, instanceName, jitConfig, owner, repo)
	if createErr != nil {
		if err := removeIdleRunner(ctx, owner, repo, instanceName); err != nil {
			log.Printf("Could not remove runner registration %s after failed VM creation: %v", instanceName, err)
		}
	}
	return createErr
}

// createRunnerInstance tries every candidate zone. A zone that is out of quota
// is skipped like any other soft failure, so one exhausted region does not hold
// up a job the rest of the pool could run. Zones pinned with a zone= label are
// tried in the order given; pool zones rotate so consecutive jobs spread out.
// If no zone accepts the job the last failure is returned, flagged as a quota
// error when at least one zone was out of quota, which leaves Cloud Tasks to
// retry once capacity frees.
func createRunnerInstance(ctx context.Context, labels *RunnerLabels, instanceName, jitConfig, owner, repo string) error {
	repoFullName := owner + "/" + repo
	cacheBucket := os.Getenv("GCRUNNER_CACHE_BUCKET")

	region := os.Getenv("GCE_REGION")
	if region == "" {
		region = "us-central1"
	}

	project := os.Getenv("GCP_PROJECT")

	// Determine zones to try
	var zones []string
	switch {
	case labels.Zone != "":
		zones = strings.Split(labels.Zone, "+")
	case len(configuredZones()) > 0:
		zones = rotate(configuredZones(), nextZoneOffset())
	default:
		var zoneErr error
		zones, zoneErr = ListZones(ctx, project, region)
		if zoneErr != nil {
			log.Printf("Failed to discover zones for %s, using fallback: %v", region, zoneErr)
			zones = []string{region + "-a", region + "-b", region + "-c"}
		}
		zones = rotate(zones, nextZoneOffset())
	}

	var lastErr error
	var outOfQuota []string
	for _, zone := range zones {
		// Resolve machine type per zone if not exact
		machineType := labels.Machine
		if labels.MachineMode != machineModeExact {
			resolved, resolveErr := ResolveMachineType(ctx, project, zone, labels)
			if resolveErr != nil {
				log.Printf("Failed to resolve machine type in %s: %v, trying next zone", zone, resolveErr)
				lastErr = resolveErr
				continue
			}
			machineType = resolved
		}

		startupScript := fmt.Sprintf(startupScriptTemplate, cacheBucket, owner, repo, registryScript(zone))
		err := insertInstance(ctx, instanceName, zone, machineType, labels, startupScript, jitConfig)
		if err == nil {
			log.Printf("Created VM %s in %s (type=%s) for %s", instanceName, zone, machineType, repoFullName)
			return nil
		}

		kind := classifyInsertError(err)
		switch kind {
		case insertErrorAlreadyExists:
			log.Printf("VM %s already exists in %s (duplicate webhook), skipping", instanceName, zone)
			return nil
		case insertErrorQuota:
			outOfQuota = append(outOfQuota, zone)
			lastErr = fmt.Errorf("failed to create VM in %s: %w", zone, err)
			log.Printf("Out of quota in %s, trying next zone", zone)
		case insertErrorFatal:
			return fmt.Errorf("failed to create VM in %s: %w", zone, err)
		default:
			lastErr = fmt.Errorf("failed to create VM in %s: %w", zone, err)
			log.Printf("Failed to create VM in %s: %v, trying next zone", zone, err)
		}
	}

	if len(outOfQuota) == len(zones) {
		return fmt.Errorf("every zone out of quota: %w", lastErr)
	}
	if len(outOfQuota) > 0 {
		return fmt.Errorf("failed to create VM in any zone (out of quota in %s): %w", strings.Join(outOfQuota, ", "), lastErr)
	}

	return fmt.Errorf("failed to create VM in any zone: %w", lastErr)
}

// configuredZones is the GCRUNNER_ZONES pool, a comma-separated list that lets
// one deployment spread across regions without a zone= on every label.
func configuredZones() []string {
	var zones []string
	for _, zone := range strings.Split(os.Getenv("GCRUNNER_ZONES"), ",") {
		if zone = strings.TrimSpace(zone); zone != "" {
			zones = append(zones, zone)
		}
	}
	return zones
}

var zoneOffset atomic.Uint64

// nextZoneOffset spreads consecutive jobs across the pool instead of stacking
// them all onto whichever zone happens to be listed first.
func nextZoneOffset() int {
	return int(zoneOffset.Add(1) - 1)
}

func rotate(zones []string, offset int) []string {
	if len(zones) < 2 {
		return zones
	}
	start := offset % len(zones)
	return append(append([]string{}, zones[start:]...), zones[:start]...)
}

// insertInstance is a seam so the zone loop can be exercised without GCE.
var insertInstance = createInstance

func createInstance(ctx context.Context, name, zone, machineType string, labels *RunnerLabels, startupScript, jitConfig string) error {
	client, err := compute.NewInstancesRESTClient(ctx)
	if err != nil {
		return fmt.Errorf("create compute client: %w", err)
	}
	defer client.Close()

	project := os.Getenv("GCP_PROJECT")
	machineType = fmt.Sprintf("zones/%s/machineTypes/%s", zone, machineType)
	sourceImage := resolveSourceImage(labels.Image)

	diskSizeGB := parseDiskSize(labels.Disk)

	instance := &computepb.Instance{
		Name:        proto.String(name),
		MachineType: proto.String(machineType),
		Disks: []*computepb.AttachedDisk{
			{
				AutoDelete: proto.Bool(true),
				Boot:       proto.Bool(true),
				InitializeParams: &computepb.AttachedDiskInitializeParams{
					SourceImage: proto.String(sourceImage),
					DiskSizeGb:  proto.Int64(diskSizeGB),
					DiskType:    proto.String(fmt.Sprintf("zones/%s/diskTypes/%s", zone, labels.DiskType)),
				},
			},
		},
		NetworkInterfaces: []*computepb.NetworkInterface{
			{
				AccessConfigs: []*computepb.AccessConfig{
					{
						Name: proto.String("External NAT"),
						Type: proto.String("ONE_TO_ONE_NAT"),
					},
				},
			},
		},
		Metadata: &computepb.Metadata{
			Items: []*computepb.Items{
				{
					Key:   proto.String("startup-script"),
					Value: proto.String(startupScript),
				},
				{
					Key:   proto.String("jit-config"),
					Value: proto.String(jitConfig),
				},
			},
		},
		Labels: map[string]string{
			"gcrunner": "true",
		},
		ServiceAccounts: []*computepb.ServiceAccount{
			{
				Email: proto.String(fmt.Sprintf("gcrunner-runner@%s.iam.gserviceaccount.com", project)),
				Scopes: []string{
					"https://www.googleapis.com/auth/cloud-platform",
				},
			},
		},
	}

	// Set spot scheduling if requested
	if labels.Spot {
		instance.Scheduling = &computepb.Scheduling{
			ProvisioningModel:         proto.String("SPOT"),
			InstanceTerminationAction: proto.String("DELETE"),
		}
	}

	op, err := client.Insert(ctx, &computepb.InsertInstanceRequest{
		Project:          project,
		Zone:             zone,
		InstanceResource: instance,
	})
	if err != nil {
		return err
	}

	// Wait for the operation to complete
	return op.Wait(ctx)
}

// findInstanceZone returns the zone holding this VM, or "" when no VM of that
// name exists. It searches the whole project, because a zone= label can place
// a VM outside the configured region or in a zone ListZones does not report.
func findInstanceZone(ctx context.Context, name string) (string, error) {
	client, err := compute.NewInstancesRESTClient(ctx)
	if err != nil {
		return "", fmt.Errorf("create compute client: %w", err)
	}
	defer client.Close()

	it := client.AggregatedList(ctx, &computepb.AggregatedListInstancesRequest{
		Project:              os.Getenv("GCP_PROJECT"),
		Filter:               proto.String(fmt.Sprintf("name = %q", name)),
		ReturnPartialSuccess: proto.Bool(true),
	})
	for {
		scope, err := it.Next()
		if err == iterator.Done {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("search for VM %s: %w", name, err)
		}
		for _, instance := range scope.Value.GetInstances() {
			if instance.GetName() == name {
				// The scope key is "zones/<zone>".
				return path.Base(scope.Key), nil
			}
		}
	}
}

func deleteRunnerVM(ctx context.Context, name string) error {
	zone, err := findInstanceZone(ctx, name)
	if err != nil {
		return err
	}
	if zone == "" {
		log.Printf("VM %s not found, may have already been deleted", name)
		return nil
	}

	client, err := compute.NewInstancesRESTClient(ctx)
	if err != nil {
		return fmt.Errorf("create compute client: %w", err)
	}
	defer client.Close()

	op, err := client.Delete(ctx, &computepb.DeleteInstanceRequest{
		Project:  os.Getenv("GCP_PROJECT"),
		Zone:     zone,
		Instance: name,
	})
	if err != nil {
		return fmt.Errorf("delete VM %s in %s: %w", name, zone, err)
	}
	if err := op.Wait(ctx); err != nil {
		return fmt.Errorf("wait for VM %s to delete in %s: %w", name, zone, err)
	}
	log.Printf("Deleted VM %s in %s", name, zone)
	return nil
}

// imageProject is where the built-in images and any image definition without
// a project of its own live.
func imageProject() string {
	if project := os.Getenv("GCRUNNER_IMAGE_PROJECT"); project != "" {
		return project
	}
	return "gcrunner-images"
}

func resolveSourceImage(image string) string {
	project := imageProject()
	imageMap := map[string]string{
		"ubuntu24-full-x64": "gcrunner-ubuntu2404-x64",
		"ubuntu22-full-x64": "gcrunner-ubuntu2204-x64",
	}
	if family, ok := imageMap[image]; ok {
		return fmt.Sprintf("projects/%s/global/images/family/%s", project, family)
	}
	if strings.Contains(image, "/") {
		return image
	}
	return fmt.Sprintf("projects/%s/global/images/%s", project, image)
}

func parseDiskSize(disk string) int64 {
	disk = strings.TrimSuffix(strings.ToLower(disk), "gb")
	var size int64
	fmt.Sscanf(disk, "%d", &size)
	if size < 10 {
		size = 50
	}
	return size
}

type insertErrorKind int

const (
	insertErrorRetryable insertErrorKind = iota
	insertErrorQuota
	insertErrorFatal
	insertErrorAlreadyExists
)

// classifyInsertError categorizes a VM creation error to decide whether to retry.
func classifyInsertError(err error) insertErrorKind {
	if err == nil {
		return insertErrorRetryable
	}
	msg := err.Error()
	if strings.Contains(msg, "QUOTA_EXCEEDED") {
		return insertErrorQuota
	}
	if strings.Contains(msg, "alreadyExists") || strings.Contains(msg, "ALREADY_EXISTS") || strings.Contains(msg, "already exists") {
		return insertErrorAlreadyExists
	}
	if strings.Contains(msg, "RESOURCE_NOT_FOUND") || strings.Contains(msg, "forbidden") || strings.Contains(msg, "Permission") {
		return insertErrorFatal
	}
	return insertErrorRetryable
}
