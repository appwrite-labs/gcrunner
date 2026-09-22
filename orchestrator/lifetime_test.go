package function

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	computepb "cloud.google.com/go/compute/apiv1/computepb"
)

// bootVM runs the production startup script with the VM's outside world
// stubbed: the metadata server and the Compute API behind curl, the runner
// behind sudo, and the clock behind sleep. The Compute API refuses the first
// refusals deletions. It returns what happened, in order: "vm-deleted" when
// the API accepted a request to delete this VM, "vm-delete-refused" when it
// did not, and "runner-exited" when the runner was gone.
func bootVM(t *testing.T, runner, clock string, refusals int) []string {
	t.Helper()
	t.Setenv("GCRUNNER_ZONES", "us-east1-b")
	t.Setenv("GCE_REGION", "europe-west1")
	t.Setenv("GCRUNNER_REGISTRY", "")
	t.Setenv("GCRUNNER_REGISTRY_PULLS", "")
	zoneOffset.Store(0)

	var script string
	withInsert(t, func(_ context.Context, _ string, instance *computepb.Instance) error {
		script = metadataItem(instance, "startup-script")
		return nil
	})
	labels := &RunnerLabels{Machine: "n2-standard-2", MachineMode: "exact"}
	if err := createRunnerInstance(context.Background(), labels, "vm", "jit", "owner", "repo"); err != nil {
		t.Fatalf("createRunnerInstance: %v", err)
	}

	home := t.TempDir()
	host := newDockerTestHost(t)
	calls := filepath.Join(home, "calls")
	deleted := filepath.Join(home, "deleted")
	if err := os.WriteFile(filepath.Join(home, "refusals"), []byte(strconv.Itoa(refusals)), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"curl": `case "$*" in
  *"Bearer token"*compute.googleapis.com/compute/v1/projects/1/zones/us-east1-b/instances/vm)
    left=$(cat "$TEST_REFUSALS")
    if [ "$left" -gt 0 ]; then
      echo $((left - 1)) > "$TEST_REFUSALS"
      echo vm-delete-refused >> "$TEST_CALLS"
      exit 22
    fi
    echo vm-deleted >> "$TEST_CALLS"
    touch "$TEST_VM_DELETED" ;;
  *compute.googleapis.com*) echo "unexpected API call: $*" >> "$TEST_CALLS"; exit 22 ;;
  */token) echo '{"access_token":"token"}' ;;
  */zone) echo projects/1/zones/us-east1-b ;;
  */name) echo vm ;;
  *) echo jit ;;
esac`,
		"sudo":    runner + "\necho runner-exited >> \"$TEST_CALLS\"",
		"sleep":   clock,
		"chown":   "exit 0",
		"install": `mkdir -p "${!#}"`,
	} {
		if err := os.WriteFile(filepath.Join(host.bin, name), []byte("#!/bin/bash\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	boot := exec.Command("bash", "-c", strings.ReplaceAll(script, "/home/runner", home))
	boot.Dir = home
	boot.Env = append(host.env, "TEST_CALLS="+calls, "TEST_VM_DELETED="+deleted, "TEST_REFUSALS="+filepath.Join(home, "refusals"))
	if out, err := boot.CombinedOutput(); err != nil {
		t.Logf("startup script exited: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

const (
	// untilRunnerExits is a clock that only moves once the runner is gone.
	untilRunnerExits = `until grep -q runner-exited "$TEST_CALLS"; do /bin/sleep 0.05; done`
	// untilDeleted is a runner that listens until the VM is gone.
	untilDeleted = `for _ in $(seq 100); do [ -e "$TEST_VM_DELETED" ] && break; /bin/sleep 0.05; done`
)

func count(calls []string, event string) int {
	total := 0
	for _, call := range calls {
		if call == event {
			total++
		}
	}
	return total
}

// A runner that ran its job, or that GitHub removed before it got one, exits;
// the VM must go with it even if the completed webhook never arrives.
func TestVMDeletesItselfWhenTheRunnerExits(t *testing.T) {
	calls := bootVM(t, `mkdir -p _diag && touch _diag/Worker_1.log`, untilRunnerExits, 0)
	if count(calls, "vm-deleted") != 1 {
		t.Errorf("VM deleted itself %d times, want once after the runner exited:\n%s", count(calls, "vm-deleted"), strings.Join(calls, "\n"))
	}
}

// A runner whose job was taken by another runner listens forever. After the
// idle window the VM deletes itself while the runner is still listening.
func TestVMDeletesItselfWhenNoJobArrives(t *testing.T) {
	calls := bootVM(t, untilDeleted, "exit 0", 0)
	if len(calls) < 2 || calls[0] != "vm-deleted" || calls[1] != "runner-exited" {
		t.Errorf("VM did not delete itself while the runner was still listening:\n%s", strings.Join(calls, "\n"))
	}
}

func TestVMDeletesItselfWhenStartupFails(t *testing.T) {
	calls := bootVM(t, `exit 1`, untilRunnerExits, 0)
	if count(calls, "vm-deleted") != 1 {
		t.Errorf("VM deleted itself %d times after the runner failed, want once:\n%s", count(calls, "vm-deleted"), strings.Join(calls, "\n"))
	}
}

// Nothing else deletes a VM whose runner never got a job, so one refused
// request must not be the end of it.
func TestVMKeepsTryingToDeleteItselfWhenTheAPIRefuses(t *testing.T) {
	calls := bootVM(t, untilDeleted, "exit 0", 2)
	if count(calls, "vm-delete-refused") != 2 || count(calls, "vm-deleted") == 0 {
		t.Errorf("VM gave up after a refused deletion:\n%s", strings.Join(calls, "\n"))
	}
}
