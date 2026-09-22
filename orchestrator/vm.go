package function

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"strings"
	"sync/atomic"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"
)

//go:embed docker-network.sh
var dockerNetworkScript string

const startupScriptTemplate = `#!/bin/bash
set -euo pipefail

METADATA_URL="http://metadata.google.internal/computeMetadata/v1"
METADATA_HEADER="Metadata-Flavor: Google"

# One attempt to delete this VM with its own service account, confirmed
# through the operation Compute Engine returns: an accepted request can
# still fail.
delete_vm() {
  local token zone name operation
  token=$(curl -sf -H "${METADATA_HEADER}" "${METADATA_URL}/instance/service-accounts/default/token" | jq -r .access_token || true)
  zone=$(curl -sf -H "${METADATA_HEADER}" "${METADATA_URL}/instance/zone" || true)
  name=$(curl -sf -H "${METADATA_HEADER}" "${METADATA_URL}/instance/name" || true)
  operation=$(curl -sf -X DELETE -H "Authorization: Bearer ${token}" \
    "https://compute.googleapis.com/compute/v1/${zone}/instances/${name}" | jq -r '.selfLink // empty' || true)
  [ -n "${operation}" ] || return 1
  curl -sf -X POST -H "Authorization: Bearer ${token}" "${operation}/wait" \
    | jq -e '.status == "DONE" and (.error | not)' >/dev/null
}

# Delete this VM on every exit, a failed startup included, so a job that
# ends without the orchestrator hearing about it does not leave the VM
# running until the lifetime cap. The runner is gone by then, so nothing
# else will delete a VM whose runner never got a job: keep trying through
# a metadata or API hiccup.
self_destruct() {
  local attempt
  for attempt in $(seq 10); do
    delete_vm && return
    sleep 30
  done
}
trap self_destruct EXIT

# Retrieve JIT config from instance metadata and delete it immediately
JIT_CONFIG=$(curl -sf -H "${METADATA_HEADER}" "${METADATA_URL}/instance/attributes/jit-config")
# Remove the metadata key so credentials are no longer queryable
curl -sf -X DELETE -H "${METADATA_HEADER}" \
  "${METADATA_URL}/instance/attributes/jit-config" || true

CACHE_BUCKET="%s"
REPO_OWNER="%s"
REPO_NAME="%s"

cd /home/runner

%s
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

# A runner whose job was cancelled, or taken by another runner in the run,
# while this VM booted would otherwise listen until the lifetime cap. The
# runner starts a worker for its job, which leaves a log behind, and a job
# can still arrive while a failed attempt waits to be retried.
(
  sleep 600
  while ! ls _diag/Worker_* >/dev/null 2>&1; do
    delete_vm && break
    sleep 30
  done
) >/dev/null 2>&1 &

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
				recordVMCreate(ctx, zone, labels.Machine, labels.Spot, outcomeUnresolved)
				lastErr = resolveErr
				continue
			}
			machineType = resolved
		}

		startupScript := fmt.Sprintf(startupScriptTemplate, cacheBucket, owner, repo, dockerNetworkScript, registryScript(zone))
		err := insertInstance(ctx, zone, runnerInstance(instanceName, zone, machineType, labels, startupScript, jitConfig))
		if err == nil {
			recordVMCreate(ctx, zone, machineType, labels.Spot, outcomeCreated)
			log.Printf("Created VM %s in %s (type=%s) for %s", instanceName, zone, machineType, repoFullName)
			return nil
		}

		kind := classifyInsertError(err)
		recordVMCreate(ctx, zone, machineType, labels.Spot, kind.outcome())
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
		case insertErrorPermanent:
			return fmt.Errorf("failed to create VM in %s: %w: %w", zone, errPermanent, err)
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

// insertInstance is a seam so the zone loop and the instance handed to GCE
// can be exercised without GCE.
var insertInstance = createInstance

func createInstance(ctx context.Context, zone string, instance *computepb.Instance) error {
	client, err := compute.NewInstancesRESTClient(ctx)
	if err != nil {
		return fmt.Errorf("create compute client: %w", err)
	}
	defer client.Close()

	op, err := client.Insert(ctx, &computepb.InsertInstanceRequest{
		Project:          os.Getenv("GCP_PROJECT"),
		Zone:             zone,
		InstanceResource: instance,
	})
	if err != nil {
		return err
	}

	// Wait for the operation to complete
	return op.Wait(ctx)
}

// runnerInstance describes the VM for one job as Compute Engine will see it.
func runnerInstance(name, zone, machineType string, labels *RunnerLabels, startupScript, jitConfig string) *computepb.Instance {
	project := os.Getenv("GCP_PROJECT")
	machineType = fmt.Sprintf("zones/%s/machineTypes/%s", zone, machineType)
	sourceImage := resolveSourceImage(labels.Image)

	diskSizeGB := parseDiskSize(labels.Disk)

	return &computepb.Instance{
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
		Scheduling: scheduling(labels),
	}
}

// GitHub stops a job after its timeout-minutes, six hours unless the workflow
// says otherwise, and stops any self-hosted job after five days. A VM alive
// longer than its job's timeout, plus the time it took to boot and register,
// is not running its job: the runner never got one, or the completed webhook
// that deletes the VM was lost.
const (
	defaultTimeout = 6 * time.Hour
	longestTimeout = 5 * 24 * time.Hour
	bootAllowance  = 15 * time.Minute
)

func lifetime(timeout string) time.Duration {
	duration, err := time.ParseDuration(timeout)
	if err != nil || duration <= 0 {
		duration = defaultTimeout
	}
	return min(duration, longestTimeout) + bootAllowance
}

// scheduling caps every VM's lifetime so Compute Engine deletes it when the
// cap passes, whatever state the runner or the orchestrator is in. Spot VMs
// carry the same termination action for preemption.
func scheduling(labels *RunnerLabels) *computepb.Scheduling {
	scheduling := &computepb.Scheduling{
		InstanceTerminationAction: proto.String("DELETE"),
		MaxRunDuration: &computepb.Duration{
			Seconds: proto.Int64(int64(lifetime(labels.Timeout).Seconds())),
		},
	}
	if labels.Spot {
		scheduling.ProvisioningModel = proto.String("SPOT")
	}
	return scheduling
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

// errPermanent marks a failure every retry repeats, in every zone. Retrying
// it holds a Cloud Tasks slot ahead of jobs that would succeed, and a few
// such jobs can stall the whole installation.
var errPermanent = errors.New("permanent failure, not retryable")

type insertErrorKind int

const (
	insertErrorRetryable insertErrorKind = iota
	insertErrorQuota
	insertErrorFatal
	insertErrorAlreadyExists
	insertErrorPermanent
)

// outcome names the failure for the metrics.
func (kind insertErrorKind) outcome() string {
	switch kind {
	case insertErrorQuota:
		return outcomeQuota
	case insertErrorFatal:
		return outcomeFatal
	case insertErrorAlreadyExists:
		return outcomeAlreadyExists
	case insertErrorPermanent:
		return outcomePermanent
	default:
		return outcomeRetryable
	}
}

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
	// A deleted image is a 400 naming the field, not RESOURCE_NOT_FOUND. Images
	// are global, so every zone answers the same. A machine type missing from
	// one zone has the same shape and stays retryable so the walk moves on.
	if strings.Contains(msg, "Invalid value for field") && strings.Contains(msg, "sourceImage") {
		return insertErrorPermanent
	}
	return insertErrorRetryable
}
