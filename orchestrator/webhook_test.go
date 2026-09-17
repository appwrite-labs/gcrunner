package function

import "testing"

func TestDeletionTarget(t *testing.T) {
	event := WorkflowJobEvent{WorkflowJob: WorkflowJob{ID: 42, RunID: 7}}

	name, ranHere := deletionTarget(event)
	if name != "gcrunner-7-42" || ranHere {
		t.Fatalf("job that never ran: got %q ran=%v, want gcrunner-7-42 ran=false", name, ranHere)
	}

	// GitHub handed this job to the VM created for job 41, which shares its labels.
	event.WorkflowJob.RunnerName = "gcrunner-7-41"
	name, ranHere = deletionTarget(event)
	if name != "gcrunner-7-41" || !ranHere {
		t.Fatalf("job that ran elsewhere: got %q ran=%v, want gcrunner-7-41 ran=true", name, ranHere)
	}

	// A job that ran on some other self-hosted runner is not ours to delete.
	event.WorkflowJob.RunnerName = "runs-on-i-0abc"
	name, ranHere = deletionTarget(event)
	if name != "gcrunner-7-42" || ranHere {
		t.Fatalf("foreign runner: got %q ran=%v, want fallback gcrunner-7-42 ran=false", name, ranHere)
	}
}
