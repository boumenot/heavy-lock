//go:build darwin

package scheduler

import (
	"bufio"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	pb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
	"github.com/boumenot/tuna/apps/tuna/jobmetrics"
	"github.com/boumenot/tuna/apps/tuna/procid"
	"github.com/boumenot/tuna/apps/tuna/storage"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func startProcess(t *testing.T, name string, args ...string) (*exec.Cmd, *pb.ProcessIdentity) {
	t.Helper()
	cmd := exec.Command(name, args...)
	return cmd, startCmd(t, cmd)
}

func startCmd(t *testing.T, cmd *exec.Cmd) *pb.ProcessIdentity {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	id, err := procid.Of(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

type jobTree struct {
	runner                         *exec.Cmd
	runnerID, rootID, descendantID *pb.ProcessIdentity
	bystanderID                    *pb.ProcessIdentity
}

// startJobTree starts a fake runner that launches the root, which starts a
// background descendant, so the root is reaped by its parent like a real
// job. After the root ends, the runner stays alive until killed.
func startJobTree(t *testing.T) jobTree {
	t.Helper()
	runner := exec.Command("/bin/sh", "-c", `/bin/sh -c '/bin/sleep 60 & echo d $!; wait' & echo r $!; wait; exec /bin/sleep 60`)
	stdout, err := runner.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	tree := jobTree{runner: runner, runnerID: startCmd(t, runner)}
	_, tree.bystanderID = startProcess(t, "/bin/sleep", "60")
	pids := map[string]int{}
	reader := bufio.NewReader(stdout)
	for len(pids) < 2 {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		fields := strings.Fields(line)
		pid, err := strconv.Atoi(fields[1])
		if err != nil {
			t.Fatal(err)
		}
		pids[fields[0]] = pid
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	}
	if tree.rootID, err = procid.Of(pids["r"]); err != nil {
		t.Fatal(err)
	}
	if tree.descendantID, err = procid.Of(pids["d"]); err != nil {
		t.Fatal(err)
	}
	return tree
}

// launchTree runs tree as a started job with a real probe and terminator,
// plus a recorded identity whose PID now belongs to another incarnation.
func launchTree(t *testing.T, tree jobTree) (*harness, string) {
	t.Helper()
	h, _ := launchHarness(t, 1, procid.Probe{})
	h.signals = procid.Terminator{}
	h.restart(1)
	req := request(t, 1)
	req.Runner.Process = tree.runnerID
	result, _, err := h.s.Submit(h.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	id := result.Job.JobId
	h.watch(req, id)
	grant := h.expectState(id, pb.JobState_JOB_STATE_RESERVED).Grant.GrantId
	if _, _, _, err := h.s.Claim(h.ctx, id, grant, req.Runner); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.s.Seal(h.ctx, id, grant, req.Runner, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.s.ReportStarted(h.ctx, id, grant, req.Runner, tree.rootID); err != nil {
		t.Fatal(err)
	}
	reused := &pb.ProcessIdentity{Pid: tree.bystanderID.Pid,
		StartTime: timestamppb.New(tree.bystanderID.StartTime.AsTime().Add(time.Microsecond))}
	if _, err := h.store.Write(h.ctx, func(tx *storage.Tx) error { return tx.TrackProcess(id, reused, "descendant") }); err != nil {
		t.Fatal(err)
	}
	if err := h.s.Discover(h.ctx, jobmetrics.DefaultSource()); err != nil {
		t.Fatal(err)
	}
	return h, id
}

// maintainUntilReleased runs the broker's maintenance steps until the job
// finishes, then checks the takeover reason and that the job processes, but
// not the mismatched identity, were killed.
func maintainUntilReleased(t *testing.T, h *harness, id string, tree jobTree, reason pb.BrokerTermination_Reason) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for h.job(id).State != pb.JobState_JOB_STATE_FINISHED {
		if time.Now().After(deadline) {
			t.Fatalf("job never released: %v", h.job(id))
		}
		if _, err := h.s.Reconcile(h.ctx); err != nil {
			t.Fatal(err)
		}
		if err := h.s.Terminate(h.ctx); err != nil {
			t.Fatal(err)
		}
		if err := h.s.Discover(h.ctx, jobmetrics.DefaultSource()); err != nil {
			t.Logf("discovery gap: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	j := h.job(id)
	if j.BrokerTermination.GetReason() != reason || h.snapshot().Status.Occupied != 0 {
		t.Fatalf("takeover = %v, occupied %d", j, h.snapshot().Status.Occupied)
	}
	for name, p := range map[string]*pb.ProcessIdentity{"root": tree.rootID, "descendant": tree.descendantID} {
		if exited, err := procid.Exited(p); err != nil || !exited {
			t.Fatalf("%s survived: exited=%v err=%v", name, exited, err)
		}
	}
	if exited, err := procid.Exited(tree.bystanderID); err != nil || exited {
		t.Fatalf("mismatched identity was signalled: exited=%v err=%v", exited, err)
	}
}

// The runner dies while its job's root and a background descendant run. The
// broker records the takeover, freezes and kills the verified job processes,
// never signals a PID whose identity does not match, and releases only after
// every tracked process is verified exited.
func TestRunnerDeathTakeoverKillsRealJobTree(t *testing.T) {
	tree := startJobTree(t)
	h, id := launchTree(t, tree)
	if err := tree.runner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = tree.runner.Wait()
	maintainUntilReleased(t, h, id, tree, pb.BrokerTermination_REASON_RUNNER_DEAD)
}

// A live runner that never acknowledges cancellation keeps running: the
// broker terminates only the job's verified processes, never the runner.
func TestUnresponsiveRunnerTakeoverKillsJobNotRunner(t *testing.T) {
	tree := startJobTree(t)
	h, id := launchTree(t, tree)
	if _, err := h.s.Cancel(h.ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	h.clock.advance(h.cfg.CancelAckTimeout)
	maintainUntilReleased(t, h, id, tree, pb.BrokerTermination_REASON_CANCEL_ACK_DEADLINE)
	if exited, err := procid.Exited(tree.runnerID); err != nil || exited {
		t.Fatalf("broker signalled the live runner: exited=%v err=%v", exited, err)
	}
}
