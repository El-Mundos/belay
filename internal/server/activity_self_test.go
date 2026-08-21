package server

import "testing"

// An agent updating itself posts its result BEFORE applying — the helper kills it, so a result sent
// afterwards would never arrive. "updated" therefore means "helper launched", not "the agent is back
// on the new version", and the tray used to close the job there: it showed success while the agent
// had not yet restarted, disagreeing with the fleet rollout, which waits for re-registration.
func TestJobs_AgentSelfUpdateStaysOpenUntilTheAgentComesBack(t *testing.T) {
	m := newJobManager()
	m.startRemoteSelf("vps", "ghcr.io/el-mundos/belay:latest", "cmd1")

	if !m.awaitRestart("cmd1", "updated") {
		t.Fatal("a successful agent self-update result must defer, not finish the job")
	}
	jobs, active := m.snapshot()
	if active != 1 {
		t.Fatalf("active jobs = %d, want 1 (the agent has not restarted yet)", active)
	}
	if jobs[0].State != "running" {
		t.Errorf("state = %q, want running", jobs[0].State)
	}
	if jobs[0].Phase != "restarting on the new image…" {
		t.Errorf("phase = %q, want the restarting phase", jobs[0].Phase)
	}

	// Re-registration is the real completion signal.
	m.finishCmd("cmd1", "updated", "", "v0.2.20", "")
	if _, active := m.snapshot(); active != 0 {
		t.Fatalf("active jobs = %d after the agent came back, want 0", active)
	}
}

// A FAILED result is final: the agent is still alive to have sent it, so there is nothing to wait
// for and deferring would hang the job until the timeout for no reason.
func TestJobs_FailedAgentSelfUpdateFinishesImmediately(t *testing.T) {
	m := newJobManager()
	m.startRemoteSelf("vps", "ghcr.io/el-mundos/belay:latest", "cmd2")

	if m.awaitRestart("cmd2", "error") {
		t.Fatal("a failed agent self-update must finish immediately, not wait for a restart")
	}
}

// An ordinary service update is unaffected: its result IS the end of the work.
func TestJobs_OrdinaryRemoteUpdateIsNotDeferred(t *testing.T) {
	m := newJobManager()
	m.startRemote("vps", "wg-easy", "wg-easy", "15.4.0", "cmd3")

	if m.awaitRestart("cmd3", "updated") {
		t.Fatal("a normal remote update must not be treated as a self-update")
	}
}

// Belay's own update job exists precisely so a failure has somewhere to show up.
func TestJobs_SelfUpdateJobIsVisibleWhileItRuns(t *testing.T) {
	m := newJobManager()
	j := m.startSelf("0.2.17", "ghcr.io/el-mundos/belay:latest")
	if j == nil {
		t.Fatal("no job created for Belay's own update")
	}
	jobs, active := m.snapshot()
	if active != 1 || jobs[0].Service != SelfUpdateService {
		t.Fatalf("self-update job not in the tray: active=%d jobs=%+v", active, jobs)
	}
	m.finish(j, "error", "helper could not reach Docker")
	if _, active := m.snapshot(); active != 0 {
		t.Fatal("job should be finished")
	}
}
