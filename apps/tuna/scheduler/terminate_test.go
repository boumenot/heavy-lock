package scheduler

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"syscall"
	"testing"

	"github.com/boumenot/heavy-lock/apps/tuna/contract"
	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"github.com/boumenot/heavy-lock/apps/tuna/storage"
)

type sentSignal struct {
	pid int64
	sig syscall.Signal
}

// fakeTerminator records signals; fail returns an error for one PID and
// signal instead of sending it.
type fakeTerminator struct {
	mu   sync.Mutex
	sent []sentSignal
	fail map[sentSignal]error
}

func (f *fakeTerminator) Signal(_ context.Context, p *pb.ProcessIdentity, sig syscall.Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail[sentSignal{p.Pid, sig}]; err != nil {
		return err
	}
	f.sent = append(f.sent, sentSignal{p.Pid, sig})
	return nil
}

func (f *fakeTerminator) take() []sentSignal {
	f.mu.Lock()
	defer f.mu.Unlock()
	sent := f.sent
	f.sent = nil
	slices.SortFunc(sent, func(a, b sentSignal) int { return int(a.pid - b.pid) })
	return sent
}

func (h *harness) terminate(want ...sentSignal) {
	h.t.Helper()
	if err := h.s.Terminate(h.ctx); err != nil {
		h.t.Fatal(err)
	}
	if got := h.signals.(*fakeTerminator).take(); !slices.Equal(got, want) {
		h.t.Fatalf("signals = %v, want %v", got, want)
	}
}

func TestTakeoverFreezesDiscoversThenKillsWithoutReleasing(t *testing.T) {
	probe := &fakeProbe{}
	h, _ := launchHarness(t, 2, probe)
	h.signals = &fakeTerminator{}
	h.restart(2)
	l := h.granted(2)
	h.start1(&l, 4242)
	if _, err := h.store.Write(h.ctx, func(tx *storage.Tx) error {
		return tx.TrackProcess(l.id, &pb.ProcessIdentity{Pid: 4243, StartTime: contract.Timestamp(5243)}, "descendant")
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Cancel(h.ctx, l.id, nil); err != nil {
		t.Fatal(err)
	}
	h.terminate() // no takeover yet: the runner owns cancellation

	if _, _, err := h.s.ReportCancelFailed(h.ctx, l.id, l.grant, l.req.Runner, "stop failed"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.job(l.id).BrokerTermination.GetReason(); got != pb.BrokerTermination_REASON_CANCEL_RUNNER_FAILED {
		t.Fatalf("takeover reason = %v", got)
	}
	runner := l.req.Runner.Process.Pid
	h.terminate(sentSignal{4242, syscall.SIGSTOP}, sentSignal{4243, syscall.SIGSTOP})
	h.terminate() // wait for a discovery pass after the freeze

	tree := discoverySnapshot(observedProcess(runner, 1, 1), observedProcess(4242, 5242, runner),
		observedProcess(4243, 5243, 4242), observedProcess(4244, 5244, 4243))
	if err := h.s.Discover(h.ctx, tree); err != nil {
		t.Fatal(err)
	}
	h.terminate(sentSignal{4244, syscall.SIGSTOP})
	if err := h.s.Discover(h.ctx, tree); err != nil {
		t.Fatal(err)
	}
	kills := []sentSignal{{4242, syscall.SIGKILL}, {4243, syscall.SIGKILL}, {4244, syscall.SIGKILL}}
	h.terminate(kills...)
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if j := h.job(l.id); j.State == pb.JobState_JOB_STATE_FINISHED || h.snapshot().Status.Occupied != 2 {
		t.Fatalf("signals released the reservation without exit evidence: %v", j)
	}

	probe.exit(4242)
	probe.exit(4243)
	h.terminate(kills...) // repeated until every process is verified exited
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.terminate(sentSignal{4244, syscall.SIGKILL})
	probe.exit(4244)
	probe.exit(runner)
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if j := h.job(l.id); j.State != pb.JobState_JOB_STATE_FINISHED || h.snapshot().Status.Occupied != 0 {
		t.Fatalf("verified exit did not release: %v occupied %d", j, h.snapshot().Status.Occupied)
	}
	h.terminate()
}

func TestTakeoverKillsAfterBoundedRoundsWithoutDiscovery(t *testing.T) {
	probe := &fakeProbe{}
	h, _ := launchHarness(t, 1, probe)
	signals := &fakeTerminator{fail: map[sentSignal]error{
		{4242, syscall.SIGSTOP}: fmt.Errorf("%w: start changed", ErrSignalMismatch)}}
	h.signals = signals
	h.restart(1)
	l := h.granted(1)
	h.start1(&l, 4242)
	if _, err := h.s.Cancel(h.ctx, l.id, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.s.ReportCancelFailed(h.ctx, l.id, l.grant, l.req.Runner, "stop failed"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.terminate() // mismatch: skipped, not retried
	for range MaxFreezeRounds - 2 {
		h.terminate()
	}
	h.terminate(sentSignal{4242, syscall.SIGKILL})
	if j := h.job(l.id); j.State == pb.JobState_JOB_STATE_FINISHED || h.snapshot().Status.Occupied != 1 {
		t.Fatalf("termination released the reservation: %v", j)
	}
}
