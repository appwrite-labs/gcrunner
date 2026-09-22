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
// behind sudo, the signal that stops its listener behind pkill, and the
// clock behind sleep. The Compute API refuses the first refusals deletions
// outright and accepts the next failures before failing them. It returns
// what happened, in order: "vm-delete-refused" and "vm-delete-failed" for
// those, "vm-deleted" once the VM was really gone, "runner-stopped" when
// the listener was told to stop and "runner-exited" when the runner was gone.
func bootVM(t *testing.T, runner, clock string, refusals, failures int) []string {
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
	for name, body := range map[string]string{
		"sudo":    runner + "\necho runner-exited >> \"$TEST_CALLS\"",
		"pkill":   `echo runner-stopped >> "$TEST_CALLS"; touch "$TEST_RUNNER_STOPPED"`,
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
	boot.Env = append(computeAPIEnvironment(t, host, home, refusals, failures), "TEST_RUNNER_STOPPED="+filepath.Join(home, "stopped"))
	if out, err := boot.CombinedOutput(); err != nil {
		t.Logf("startup script exited: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(home, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

// computeAPI is the metadata server and the Compute API as the startup script
// sees them through curl: a token, this VM's zone and name, its JIT config,
// and a deletion that is accepted, refused, or accepted and then fails, as
// computeAPIEnvironment arranged.
const computeAPI = `consume() { local left; left=$(cat "$1"); [ "${left:-0}" -gt 0 ] && echo $((left - 1)) > "$1"; }
case "$*" in
  *"Bearer token"*compute.googleapis.com/compute/v1/projects/1/zones/us-east1-b/instances/vm)
    if consume "$TEST_REFUSALS"; then echo vm-delete-refused >> "$TEST_CALLS"; exit 22; fi
    echo '{"selfLink":"https://www.googleapis.com/compute/v1/projects/1/zones/us-east1-b/operations/delete-vm"}' ;;
  *"Bearer token"*projects/1/zones/us-east1-b/operations/delete-vm/wait)
    if consume "$TEST_FAILURES"; then echo vm-delete-failed >> "$TEST_CALLS"; echo '{"status":"DONE","error":{"errors":[{"code":"RESOURCE_NOT_READY"}]}}'; exit 0; fi
    echo vm-deleted >> "$TEST_CALLS"
    touch "$TEST_VM_DELETED"
    echo '{"status":"DONE"}' ;;
  *compute.googleapis.com*|*googleapis.com/compute*) echo "unexpected API call: $*" >> "$TEST_CALLS"; exit 22 ;;
  */token) echo '{"access_token":"token"}' ;;
  */zone) echo projects/1/zones/us-east1-b ;;
  */name) echo vm ;;
  *) echo jit ;;
esac`

// computeAPIEnvironment installs computeAPI as curl and returns the
// environment it needs, with the API set to refuse the first refusals
// deletions and fail the next failures after accepting them.
func computeAPIEnvironment(t *testing.T, host dockerTestHost, home string, refusals, failures int) []string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(host.bin, "curl"), []byte("#!/bin/bash\n"+computeAPI+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := append(host.env, "TEST_CALLS="+filepath.Join(home, "calls"), "TEST_VM_DELETED="+filepath.Join(home, "deleted"))
	for name, left := range map[string]int{"refusals": refusals, "failures": failures} {
		file := filepath.Join(home, name)
		if err := os.WriteFile(file, []byte(strconv.Itoa(left)), 0o644); err != nil {
			t.Fatal(err)
		}
		env = append(env, "TEST_"+strings.ToUpper(name)+"="+file)
	}
	return env
}

const (
	// untilRunnerExits is a clock that only moves once the runner is gone.
	untilRunnerExits = `until grep -q runner-exited "$TEST_CALLS"; do /bin/sleep 0.05; done`
	// untilJobStarts is a clock that only moves once the runner has a job.
	untilJobStarts = `until grep -qs "Running job" runner.log; do /bin/sleep 0.05; done`
	// untilStopped is a runner that listens until it is told to stop.
	untilStopped = `for _ in $(seq 100); do [ -e "$TEST_RUNNER_STOPPED" ] && break; /bin/sleep 0.05; done`
	// ranJob is a runner that was assigned its job and finished it.
	ranJob = `echo "2026-09-22 10:00:00Z: Running job: CI"`
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

func index(calls []string, event string) int {
	for i, call := range calls {
		if call == event {
			return i
		}
	}
	return -1
}

// A runner that ran its job, or that GitHub removed before it got one, exits;
// the VM must go with it even if the completed webhook never arrives.
func TestVMDeletesItselfWhenTheRunnerExits(t *testing.T) {
	calls := bootVM(t, ranJob, untilRunnerExits, 0, 0)
	if count(calls, "vm-deleted") != 1 || count(calls, "runner-stopped") != 0 {
		t.Errorf("want the VM deleted once after the runner exited on its own:\n%s", strings.Join(calls, "\n"))
	}
}

// A runner whose job was taken by another runner listens forever. After the
// idle window its listener is stopped, so no job can be assigned while the
// VM is deleted, and the VM is deleted only once the runner is gone.
func TestVMStopsAnIdleRunnerBeforeDeletingItself(t *testing.T) {
	calls := bootVM(t, untilStopped, "exit 0", 0, 0)
	stopped, exited, deleted := index(calls, "runner-stopped"), index(calls, "runner-exited"), index(calls, "vm-deleted")
	if stopped < 0 || exited < stopped || deleted < exited {
		t.Errorf("want the listener stopped, then the runner gone, then the VM deleted:\n%s", strings.Join(calls, "\n"))
	}
}

// The idle window passing after a job started must leave the runner alone.
func TestVMLeavesARunnerWithAJobAlone(t *testing.T) {
	calls := bootVM(t, ranJob+"\n"+untilStopped, untilJobStarts, 0, 0)
	if count(calls, "runner-stopped") != 0 || count(calls, "vm-deleted") != 1 {
		t.Errorf("want the runner left to its job and the VM deleted once it exited:\n%s", strings.Join(calls, "\n"))
	}
}

func TestVMDeletesItselfWhenStartupFails(t *testing.T) {
	calls := bootVM(t, `exit 1`, untilRunnerExits, 0, 0)
	if count(calls, "vm-deleted") != 1 {
		t.Errorf("VM deleted itself %d times after the runner failed, want once:\n%s", count(calls, "vm-deleted"), strings.Join(calls, "\n"))
	}
}

// Nothing else deletes a VM whose runner never got a job, so neither a
// refused request nor an accepted one that then fails is the end of it.
func TestVMKeepsTryingToDeleteItselfUntilItIsGone(t *testing.T) {
	calls := bootVM(t, ranJob, "exit 0", 2, 1)
	if count(calls, "vm-delete-refused") != 2 || count(calls, "vm-delete-failed") != 1 || count(calls, "vm-deleted") != 1 {
		t.Errorf("VM gave up before it was gone:\n%s", strings.Join(calls, "\n"))
	}
}
