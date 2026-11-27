package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/boumenot/tuna/apps/tuna/contract"
	pb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
	"github.com/boumenot/tuna/apps/tuna/jobmetrics"
	"github.com/boumenot/tuna/apps/tuna/storage"
)

type discoverySource func(context.Context) (jobmetrics.Snapshot, error)

func (s discoverySource) Snapshot(ctx context.Context) (jobmetrics.Snapshot, error) {
	return s(ctx)
}

func discoverySnapshot(processes ...jobmetrics.Process) discoverySource {
	return func(context.Context) (jobmetrics.Snapshot, error) {
		return jobmetrics.Snapshot{Processes: processes}, nil
	}
}

func observedProcess(pid, start, parent int64) jobmetrics.Process {
	return jobmetrics.Process{
		Identity:  jobmetrics.Identity{PID: pid, StartTimeUnixNs: start},
		ParentPID: parent,
	}
}

func (h *harness) tracked(id string) []storage.Process {
	h.t.Helper()
	var processes []storage.Process
	err := h.store.View(h.ctx, func(r *storage.Reader) error {
		var err error
		processes, err = r.Processes(id)
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return processes
}

func hasTracked(processes []storage.Process, pid int64, role string) bool {
	for _, p := range processes {
		if p.Identity.Pid == pid && p.Role == role {
			return true
		}
	}
	return false
}

func TestDiscoveryPersistsDescendantsAcrossRestartAndReparenting(t *testing.T) {
	probe := &fakeProbe{}
	h, _ := launchHarness(t, 2, probe)
	l := h.granted(2)
	h.start1(&l, 4242)
	root := observedProcess(4242, 5242, 1)
	child := observedProcess(4243, 5243, 4242)
	child.Problem = "RSS unavailable"
	grandchild := observedProcess(4244, 5244, 4243)
	if err := h.s.Discover(h.ctx, discoverySnapshot(grandchild, root, child)); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int64{4243, 4244} {
		if !hasTracked(h.tracked(l.id), pid, "descendant") {
			t.Fatalf("stable descendant %d was not persisted", pid)
		}
	}

	h.restart(2)
	probe.exit(root.Identity.PID)
	j, _, err := h.s.ReportFinished(h.ctx, l.id, l.grant, l.req.Runner, l.proc, exited(0))
	if err != nil || j.State == pb.JobState_JOB_STATE_FINISHED {
		t.Fatalf("restart released before discovery: job=%v err=%v", j, err)
	}
	child.ParentPID = 1
	greatGrandchild := observedProcess(4245, 5245, 4244)
	if err := h.s.Discover(h.ctx, discoverySnapshot(greatGrandchild, grandchild, child)); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int64{4243, 4244, 4245} {
		if !hasTracked(h.tracked(l.id), pid, "descendant") {
			t.Fatalf("restart or reparenting lost descendant %d", pid)
		}
	}
	if err := h.s.Discover(h.ctx, discoverySnapshot()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if j := h.job(l.id); j.State == pb.JobState_JOB_STATE_FINISHED {
		t.Fatal("omitting known descendants was treated as exit evidence")
	}
	for _, pid := range []int64{4243, 4244, 4245} {
		probe.exit(pid)
	}
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.expectState(l.id, pb.JobState_JOB_STATE_FINISHED)
}

func TestDiscoveryRejectsIdentityGapsWithoutReleasing(t *testing.T) {
	root := observedProcess(4242, 5242, 1)
	child := observedProcess(4243, 5243, 4242)
	grandchild := observedProcess(4244, 5244, 4243)
	unstable := child
	unstable.Unstable = true
	unstableRoot := root
	unstableRoot.Unstable = true
	unstableReusedRoot := unstableRoot
	unstableReusedRoot.Identity.StartTimeUnixNs = 9000
	invalidRoot := root
	invalidRoot.Identity.StartTimeUnixNs = 0
	invalid := child
	invalid.Identity.StartTimeUnixNs = 0
	invalidPID := child
	invalidPID.Identity.PID = 1 << 31
	for _, scenario := range []struct {
		name   string
		source jobmetrics.Source
	}{
		{name: "missing source"},
		{name: "enumeration failure", source: discoverySource(func(context.Context) (jobmetrics.Snapshot, error) {
			return jobmetrics.Snapshot{}, errors.New("enumeration denied")
		})},
		{name: "identity verification failure", source: discoverySource(func(context.Context) (jobmetrics.Snapshot, error) {
			return jobmetrics.Snapshot{IdentityFailure: "verification denied"}, nil
		})},
		{name: "unstable anchor", source: discoverySnapshot(unstableRoot, child, grandchild)},
		{name: "unstable reused anchor", source: discoverySnapshot(unstableReusedRoot, child, grandchild)},
		{name: "invalid anchor", source: discoverySnapshot(invalidRoot, child, grandchild)},
		{name: "duplicate anchor", source: discoverySnapshot(root, root, child, grandchild)},
		{name: "unstable child", source: discoverySnapshot(root, unstable, grandchild)},
		{name: "duplicate child", source: discoverySnapshot(root, child, child, grandchild)},
		{name: "invalid child", source: discoverySnapshot(root, invalid, grandchild)},
		{name: "invalid child PID", source: discoverySnapshot(root, invalidPID)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			probe := &fakeProbe{}
			h, _ := launchHarness(t, 2, probe)
			l := h.granted(2)
			h.start1(&l, root.Identity.PID)
			if err := h.s.Discover(h.ctx, discoverySnapshot()); err != nil {
				t.Fatal(err)
			}
			if err := h.s.Discover(h.ctx, scenario.source); err == nil {
				t.Fatal("identity gap was not reported")
			}
			for _, pid := range []int64{4243, 4244} {
				if hasTracked(h.tracked(l.id), pid, "descendant") {
					t.Fatalf("untrusted descendant %d was persisted", pid)
				}
			}
			probe.exit(root.Identity.PID)
			j, _, err := h.s.ReportFinished(h.ctx, l.id, l.grant, l.req.Runner, l.proc, exited(0))
			if err != nil || j.State == pb.JobState_JOB_STATE_FINISHED {
				t.Fatalf("failed scan released capacity: job=%v err=%v", j, err)
			}
			if err := h.s.Discover(h.ctx, discoverySnapshot()); err != nil {
				t.Fatal(err)
			}
			if _, err := h.s.Reconcile(h.ctx); err != nil {
				t.Fatal(err)
			}
			h.expectState(l.id, pb.JobState_JOB_STATE_FINISHED)
		})
	}
}

func TestDiscoveryPersistsStableSiblingsWhenAnotherIdentityIsUnstable(t *testing.T) {
	probe := &fakeProbe{}
	h, _ := launchHarness(t, 2, probe)
	l := h.granted(2)
	h.start1(&l, 4242)
	root := observedProcess(4242, 5242, 1)
	stable := observedProcess(4243, 5243, 4242)
	unstable := observedProcess(4244, 5244, 4242)
	unstable.Unstable = true
	if err := h.s.Discover(h.ctx, discoverySnapshot(root, stable, unstable)); err == nil {
		t.Fatal("unstable sibling was not reported")
	}
	if !hasTracked(h.tracked(l.id), stable.Identity.PID, "descendant") ||
		hasTracked(h.tracked(l.id), unstable.Identity.PID, "descendant") {
		t.Fatalf("stable/unstable siblings were not handled separately: %+v", h.tracked(l.id))
	}
	probe.exit(root.Identity.PID)
	j, _, err := h.s.ReportFinished(h.ctx, l.id, l.grant, l.req.Runner, l.proc, exited(0))
	if err != nil || j.State == pb.JobState_JOB_STATE_FINISHED {
		t.Fatalf("identity gap released capacity: job=%v err=%v", j, err)
	}
	if err := h.s.Discover(h.ctx, discoverySnapshot()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if j := h.job(l.id); j.State == pb.JobState_JOB_STATE_FINISHED {
		t.Fatal("omitting the stable sibling erased its durable identity")
	}
	probe.exit(stable.Identity.PID)
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.expectState(l.id, pb.JobState_JOB_STATE_FINISHED)
}

func TestDiscoveryDoesNotFollowReusedAnchorPID(t *testing.T) {
	h, _ := launchHarness(t, 2, &fakeProbe{})
	l := h.granted(2)
	h.start1(&l, 4242)
	reusedRoot := observedProcess(4242, 9000, 1)
	child := observedProcess(4243, 9001, 4242)
	if err := h.s.Discover(h.ctx, discoverySnapshot(reusedRoot, child)); err != nil {
		t.Fatal(err)
	}
	if hasTracked(h.tracked(l.id), child.Identity.PID, "descendant") {
		t.Fatal("discovery followed a different incarnation of the root PID")
	}
}

func TestDiscoveryAllowsMetricsOnlyGaps(t *testing.T) {
	h, _ := launchHarness(t, 2, &fakeProbe{})
	l := h.granted(2)
	h.start1(&l, 4242)
	root := observedProcess(4242, 5242, 1)
	child := observedProcess(4243, 5243, 4242)
	root.Problem, child.Problem = "CPU unavailable", "RSS unavailable"
	source := discoverySource(func(context.Context) (jobmetrics.Snapshot, error) {
		return jobmetrics.Snapshot{Processes: []jobmetrics.Process{root, child},
			IncompletenessReason: "process metrics unavailable"}, nil
	})
	if err := h.s.Discover(h.ctx, source); err != nil {
		t.Fatalf("metrics-only gap blocked identity discovery: %v", err)
	}
	if !hasTracked(h.tracked(l.id), child.Identity.PID, "descendant") {
		t.Fatal("metrics-only gap discarded a stable child identity")
	}
}

func TestDiscoverySerializesCaptureWithReleaseWithoutBlockingCancellation(t *testing.T) {
	probe := &fakeProbe{}
	h, _ := launchHarness(t, 2, probe)
	l := h.granted(2)
	h.start1(&l, 4242)
	if err := h.s.Discover(h.ctx, discoverySnapshot()); err != nil {
		t.Fatal(err)
	}
	root := observedProcess(4242, 5242, 1)
	child := observedProcess(4243, 5243, 4242)
	entered, resume := make(chan struct{}), make(chan struct{})
	var resumeOnce sync.Once
	resumeScan := func() { resumeOnce.Do(func() { close(resume) }) }
	done := make(chan error, 1)
	scanFinished := false
	defer func() {
		resumeScan()
		if !scanFinished {
			<-done
		}
	}()
	go func() {
		done <- h.s.Discover(h.ctx, discoverySource(func(context.Context) (jobmetrics.Snapshot, error) {
			close(entered)
			<-resume
			return jobmetrics.Snapshot{Processes: []jobmetrics.Process{root, child}}, nil
		}))
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("discovery did not start")
	}
	probe.exit(root.Identity.PID)
	finished := make(chan error, 1)
	go func() {
		j, _, err := h.s.ReportFinished(h.ctx, l.id, l.grant, l.req.Runner, l.proc, exited(0))
		if err == nil && j.State == pb.JobState_JOB_STATE_FINISHED {
			err = errors.New("finish released while a positive scan was in flight")
		}
		finished <- err
	}()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("finish was blocked behind process enumeration")
	}
	cancelled := make(chan error, 1)
	go func() {
		_, err := h.s.Cancel(h.ctx, l.id, nil)
		cancelled <- err
	}()
	select {
	case err := <-cancelled:
		if err != nil {
			t.Fatalf("cancel during scan: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation was blocked behind process enumeration")
	}
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.expectState(l.id, pb.JobState_JOB_STATE_FINISHING)
	resumeScan()
	err := <-done
	scanFinished = true
	if err != nil {
		t.Fatal(err)
	}
	if !hasTracked(h.tracked(l.id), child.Identity.PID, "descendant") {
		t.Fatal("positive observation was not durable before the barrier opened")
	}
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.expectState(l.id, pb.JobState_JOB_STATE_FINISHING)
	probe.exit(child.Identity.PID)
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.expectState(l.id, pb.JobState_JOB_STATE_FINISHED)
}

func TestDiscoveryDoesNotCoverRootsReportedAfterScanBegins(t *testing.T) {
	probe := &fakeProbe{}
	h, _ := launchHarness(t, 2, probe)
	first := h.granted(1)
	h.start1(&first, 4242)
	entered, resume := make(chan struct{}), make(chan struct{})
	var resumeOnce sync.Once
	resumeScan := func() { resumeOnce.Do(func() { close(resume) }) }
	done := make(chan error, 1)
	scanFinished := false
	defer func() {
		resumeScan()
		if !scanFinished {
			<-done
		}
	}()
	go func() {
		done <- h.s.Discover(h.ctx, discoverySource(func(context.Context) (jobmetrics.Snapshot, error) {
			close(entered)
			<-resume
			return jobmetrics.Snapshot{}, nil
		}))
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("discovery did not start")
	}
	second := h.granted(1)
	if _, already, _, err := h.s.Claim(h.ctx, second.id, second.grant, second.req.Runner); err != nil || already {
		t.Fatalf("claim: already=%v err=%v", already, err)
	}
	if _, _, err := h.s.Seal(h.ctx, second.id, second.grant, second.req.Runner, false); err != nil {
		t.Fatal(err)
	}
	second.proc = &pb.ProcessIdentity{Pid: 4342, StartTime: contract.Timestamp(5342)}
	if _, _, err := h.s.ReportStarted(h.ctx, second.id, second.grant, second.req.Runner, second.proc); err != nil {
		t.Fatal(err)
	}
	probe.exit(second.proc.Pid)
	j, _, err := h.s.ReportFinished(h.ctx, second.id, second.grant, second.req.Runner, second.proc, exited(0))
	if err != nil || j.State == pb.JobState_JOB_STATE_FINISHED {
		t.Fatalf("new root was covered by an earlier scan: job=%v err=%v", j, err)
	}
	resumeScan()
	err = <-done
	scanFinished = true
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.expectState(second.id, pb.JobState_JOB_STATE_FINISHING)
	if err := h.s.Discover(h.ctx, discoverySnapshot()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.expectState(second.id, pb.JobState_JOB_STATE_FINISHED)
}

func TestDiscoveryFinishesBeforeCleanStop(t *testing.T) {
	h, _ := launchHarness(t, 2, &fakeProbe{})
	l := h.granted(2)
	h.start1(&l, 4242)
	entered, resume := make(chan struct{}), make(chan struct{})
	var resumeOnce sync.Once
	resumeScan := func() { resumeOnce.Do(func() { close(resume) }) }
	discovered := make(chan error, 1)
	scanFinished := false
	defer func() {
		resumeScan()
		if !scanFinished {
			<-discovered
		}
	}()
	go func() {
		discovered <- h.s.Discover(h.ctx, discoverySource(func(context.Context) (jobmetrics.Snapshot, error) {
			close(entered)
			<-resume
			return jobmetrics.Snapshot{Processes: []jobmetrics.Process{
				observedProcess(4242, 5242, 1), observedProcess(4243, 5243, 4242),
			}}, nil
		}))
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("discovery did not start")
	}
	stopping, stopped := make(chan struct{}), make(chan error, 1)
	go func() {
		close(stopping)
		stopped <- h.s.Stop(h.ctx)
	}()
	<-stopping
	select {
	case err := <-stopped:
		t.Fatalf("clean stop overtook an in-flight observation: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	resumeScan()
	err := <-discovered
	scanFinished = true
	if err != nil {
		t.Fatal(err)
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	h.restart(2)
	if !hasTracked(h.tracked(l.id), 4243, "descendant") {
		t.Fatal("clean restart lost the captured descendant")
	}
}

func TestDiscoveryRefreshesPreviouslyVerifiedIdentity(t *testing.T) {
	probe := &fakeProbe{}
	h, _ := launchHarness(t, 2, probe)
	l := h.granted(2)
	h.start1(&l, 4242)
	child := &pb.ProcessIdentity{Pid: 4243, StartTime: contract.Timestamp(5243)}
	if _, err := h.store.Write(h.ctx, func(tx *storage.Tx) error {
		if err := tx.TrackProcess(l.id, child, "descendant"); err != nil {
			return err
		}
		return tx.VerifyProcessExited(l.id, child)
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.s.Discover(h.ctx, discoverySnapshot(
		observedProcess(4242, 5242, 1), observedProcess(4243, 5243, 4242))); err != nil {
		t.Fatal(err)
	}
	for _, p := range h.tracked(l.id) {
		if p.Identity.Pid == child.Pid && p.ExitedVerifiedAt != nil {
			t.Fatalf("live descendant retained stale exit evidence: %+v", p)
		}
	}
	probe.exit(l.proc.Pid)
	j, _, err := h.s.ReportFinished(h.ctx, l.id, l.grant, l.req.Runner, l.proc, exited(0))
	if err != nil || j.State == pb.JobState_JOB_STATE_FINISHED {
		t.Fatalf("stale exit evidence released capacity: job=%v err=%v", j, err)
	}
}

func TestDiscoveryPersistsPositiveSnapshotAfterCallerCancellation(t *testing.T) {
	h, _ := launchHarness(t, 2, &fakeProbe{})
	l := h.granted(2)
	h.start1(&l, 4242)
	ctx, cancel := context.WithCancel(h.ctx)
	source := discoverySource(func(context.Context) (jobmetrics.Snapshot, error) {
		cancel()
		return jobmetrics.Snapshot{Processes: []jobmetrics.Process{
			observedProcess(4242, 5242, 1), observedProcess(4243, 5243, 4242),
		}}, nil
	})
	if err := h.s.Discover(ctx, source); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !hasTracked(h.tracked(l.id), 4243, "descendant") {
		t.Fatalf("caller cancellation discarded captured descendant for %s", l.id)
	}
}
