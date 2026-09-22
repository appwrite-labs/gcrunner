package function

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	computepb "cloud.google.com/go/compute/apiv1/computepb"
)

// bootVM runs the production startup script with the VM's outside world
// stubbed: the metadata server and the Compute API behind curl, the runner
// behind sudo, and the idle watchdog's clock behind sleep. It returns what the
// script called, in order, with the runner's own exit marked "runner-exited".
func bootVM(t *testing.T, runner, clock string) []string {
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
	for name, body := range map[string]string{
		"curl": `echo "curl $*" >> "$TEST_CALLS"
case "$*" in
  *compute.googleapis.com*) touch "$TEST_VM_DELETED" ;;
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
	boot.Env = append(host.env, "TEST_CALLS="+calls, "TEST_VM_DELETED="+deleted)
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
	deleteCall = "curl -sf -X DELETE -H Authorization: Bearer token https://compute.googleapis.com/compute/v1/projects/1/zones/us-east1-b/instances/vm"
	// untilRunnerExits is a clock that only moves once the runner is gone.
	untilRunnerExits = `until grep -q runner-exited "$TEST_CALLS"; do /bin/sleep 0.05; done`
)

func deletes(calls []string) int {
	count := 0
	for _, call := range calls {
		if call == deleteCall {
			count++
		}
	}
	return count
}

// A runner that ran its job, or that GitHub removed before it got one, exits;
// the VM must go with it even if the completed webhook never arrives.
func TestVMDeletesItselfWhenTheRunnerExits(t *testing.T) {
	calls := bootVM(t, `mkdir -p _diag && touch _diag/Worker_1.log`, untilRunnerExits)
	if deletes(calls) != 1 {
		t.Errorf("VM deleted itself %d times, want once after the runner exited:\n%s", deletes(calls), strings.Join(calls, "\n"))
	}
}

// A runner whose job was taken by another runner listens forever. After the
// idle window the VM deletes itself while the runner is still listening.
func TestVMDeletesItselfWhenNoJobArrives(t *testing.T) {
	calls := bootVM(t, `for _ in $(seq 100); do [ -e "$TEST_VM_DELETED" ] && break; /bin/sleep 0.05; done`, "exit 0")
	exited := -1
	for i, call := range calls {
		if call == "runner-exited" {
			exited = i
		}
	}
	if exited < 0 || calls[exited-1] != deleteCall {
		t.Errorf("VM did not delete itself while the runner was still listening:\n%s", strings.Join(calls, "\n"))
	}
}

func TestVMDeletesItselfWhenStartupFails(t *testing.T) {
	calls := bootVM(t, `exit 1`, untilRunnerExits)
	if deletes(calls) != 1 {
		t.Errorf("VM deleted itself %d times after the runner failed, want once:\n%s", deletes(calls), strings.Join(calls, "\n"))
	}
}
