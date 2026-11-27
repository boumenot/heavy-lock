package scheduler

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/boumenot/heavy-lock/apps/tuna/contract"
	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"github.com/boumenot/heavy-lock/apps/tuna/storage"
	"google.golang.org/protobuf/proto"
)

// fakeProbe reports process exit only for identities marked exited.
type fakeProbe struct {
	mu     sync.Mutex
	exited map[int64]bool
	err    error
}

func (p *fakeProbe) Exited(_ context.Context, id *pb.ProcessIdentity) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exited[id.Pid], p.err
}

func (p *fakeProbe) exit(pid int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exited == nil {
		p.exited = map[int64]bool{}
	}
	p.exited[pid] = true
}

func (p *fakeProbe) setError(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
}

func launchHarness(t *testing.T, capacity uint32, probe ProcessProbe) (*harness, *[][]*pb.BrokerEvent) {
	t.Helper()
	cfg := DefaultConfig(capacity)
	cfg.Settle = 0
	var published [][]*pb.BrokerEvent
	h := &harness{t: t, ctx: context.Background(), path: filepath.Join(t.TempDir(), "state.db"), cfg: cfg,
		sampler: &fakeSampler{sample: idle}, probe: probe,
		publish: func(batch []*pb.BrokerEvent) { published = append(published, batch) }}
	h.start()
	t.Cleanup(func() { h.store.Close() })
	return h, &published
}

type launched struct {
	req   *pb.SubmitJobRequest
	id    string
	grant string
	proc  *pb.ProcessIdentity
}

func (h *harness) granted(weight uint32) launched {
	h.t.Helper()
	req, id, _ := h.queued(weight)
	j := h.expectState(id, pb.JobState_JOB_STATE_RESERVED)
	return launched{req: req, id: id, grant: j.Grant.GrantId}
}

func (h *harness) start1(l *launched, pid int64) {
	h.t.Helper()
	if _, already, _, err := h.s.Claim(h.ctx, l.id, l.grant, l.req.Runner); err != nil || already {
		h.t.Fatalf("claim: already=%v err=%v", already, err)
	}
	if _, _, err := h.s.Seal(h.ctx, l.id, l.grant, l.req.Runner, false); err != nil {
		h.t.Fatalf("seal: %v", err)
	}
	l.proc = &pb.ProcessIdentity{Pid: pid, StartTime: contract.Timestamp(1000 + pid)}
	if _, _, err := h.s.ReportStarted(h.ctx, l.id, l.grant, l.req.Runner, l.proc); err != nil {
		h.t.Fatalf("started: %v", err)
	}
	if err := h.s.Discover(h.ctx, discoverySnapshot()); err != nil {
		h.t.Fatalf("initial discovery: %v", err)
	}
}

func exited(code int64) *pb.Outcome {
	return &pb.Outcome{Kind: pb.OutcomeKind_OUTCOME_KIND_EXITED, ExitCode: proto.Int64(code)}
}

func TestClaimIsOneShotAndBoundToIncarnation(t *testing.T) {
	h, _ := launchHarness(t, 2, &fakeProbe{})
	l := h.granted(2)
	other := &pb.RunnerIncarnation{IncarnationId: uuid4(t), Process: l.req.Runner.Process}
	if _, _, _, err := h.s.Claim(h.ctx, l.id, l.grant, other); !errors.Is(err, storage.ErrIdentity) {
		t.Fatalf("replacement incarnation claim err = %v, want identity mismatch", err)
	}
	if _, already, _, err := h.s.Claim(h.ctx, l.id, l.grant, l.req.Runner); err != nil || already {
		t.Fatalf("first claim: already=%v err=%v", already, err)
	}
	// A retransmitted claim is acknowledged, not a second launch allowance.
	if _, already, _, err := h.s.Claim(h.ctx, l.id, l.grant, l.req.Runner); err != nil || !already {
		t.Fatalf("duplicate claim: already=%v err=%v", already, err)
	}
	if _, _, _, err := h.s.Claim(h.ctx, l.id, uuid4(t), l.req.Runner); err == nil {
		t.Fatal("claim under a different grant succeeded")
	}
	if _, _, err := h.s.Seal(h.ctx, l.id, l.grant, other, false); !errors.Is(err, storage.ErrIdentity) {
		t.Fatalf("replacement seal err = %v", err)
	}
}

func TestFinishReleasesOnlyAfterVerifiedExit(t *testing.T) {
	probe := &fakeProbe{}
	h, _ := launchHarness(t, 2, probe)
	l := h.granted(2)
	_, next, _ := h.queued(2)
	h.start1(&l, 4242)
	if j := h.job(l.id); j.State != pb.JobState_JOB_STATE_RUNNING {
		t.Fatalf("state after start = %v", j.State)
	}
	j, _, err := h.s.ReportFinished(h.ctx, l.id, l.grant, l.req.Runner, l.proc, exited(3))
	if err != nil {
		t.Fatal(err)
	}
	if j.State == pb.JobState_JOB_STATE_FINISHED {
		t.Fatal("released without OS evidence that the command exited")
	}
	h.expectState(next, pb.JobState_JOB_STATE_QUEUED)
	// A lost reply is retransmitted; the duplicate report is harmless.
	if _, _, err := h.s.ReportFinished(h.ctx, l.id, l.grant, l.req.Runner, l.proc, exited(3)); err != nil {
		t.Fatalf("duplicate finish report: %v", err)
	}
	probe.exit(4242)
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	j = h.expectState(l.id, pb.JobState_JOB_STATE_FINISHED)
	if j.Outcome.GetKind() != pb.OutcomeKind_OUTCOME_KIND_EXITED || j.Outcome.GetExitCode() != 3 {
		t.Fatalf("outcome = %v", j.Outcome)
	}
	h.expectState(next, pb.JobState_JOB_STATE_RESERVED)
}

func TestFinishSummaryIsAtomicAndConflictsDoNotOverwrite(t *testing.T) {
	h, _ := launchHarness(t, 2, &fakeProbe{})
	l := h.granted(2)
	h.start1(&l, 4242)
	invalid := &pb.ResourceSummary{Elapsed: contract.MillisecondDuration(500)}
	if _, _, err := h.s.ReportFinishedWithResources(h.ctx, l.id, l.grant, l.req.Runner, l.proc, exited(0), invalid); err == nil {
		t.Fatal("invalid summary was accepted")
	}
	if job := h.job(l.id); job.Outcome != nil || job.Resources != nil || job.State != pb.JobState_JOB_STATE_RUNNING {
		t.Fatalf("failed summary write committed a partial finish: %+v", job)
	}
	summary := &pb.ResourceSummary{Elapsed: contract.MillisecondDuration(500), CpuTime: contract.MillisecondDuration(200),
		Completeness: pb.MeasurementCompleteness_MEASUREMENT_COMPLETENESS_PARTIAL}
	if _, _, err := h.s.ReportFinishedWithResources(h.ctx, l.id, l.grant, l.req.Runner, l.proc, exited(0), summary); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.s.ReportFinishedWithResources(h.ctx, l.id, l.grant, l.req.Runner, l.proc, exited(0), summary); err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	conflict := proto.Clone(summary).(*pb.ResourceSummary)
	conflict.Elapsed = contract.MillisecondDuration(501)
	if _, _, err := h.s.ReportFinishedWithResources(h.ctx, l.id, l.grant, l.req.Runner, l.proc, exited(0), conflict); !errors.Is(err, storage.ErrIdentity) {
		t.Fatalf("conflicting runtime: %v", err)
	}
	if _, _, err := h.s.ReportFinishedWithResources(h.ctx, l.id, l.grant, l.req.Runner, l.proc, exited(1), summary); !errors.Is(err, storage.ErrIdentity) {
		t.Fatalf("conflicting outcome: %v", err)
	}
	if job := h.job(l.id); job.Outcome.GetExitCode() != 0 || !proto.Equal(job.Resources, summary) {
		t.Fatalf("rejected report overwrote the outcome or summary: %+v", job)
	}
}

func TestMissingProbeFailsClosed(t *testing.T) {
	h, _ := launchHarness(t, 2, nil)
	var warnings []string
	h.s.warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	l := h.granted(2)
	h.start1(&l, 77)
	if _, _, err := h.s.ReportFinished(h.ctx, l.id, l.grant, l.req.Runner, l.proc, exited(0)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if j := h.job(l.id); j.State == pb.JobState_JOB_STATE_FINISHED {
		t.Fatal("released without a process probe")
	}
	if got := strings.Join(warnings, "\n"); !strings.Contains(got, "process probe is unavailable") ||
		!strings.Contains(got, "runner") || !strings.Contains(got, "command") {
		t.Fatalf("missing process probe was not reported: %s", got)
	}
}

func TestProbeErrorKeepsReservationHeld(t *testing.T) {
	probe := &fakeProbe{err: errors.New("sysctl failed")}
	probe.exit(9)
	h, _ := launchHarness(t, 2, probe)
	l := h.granted(2)
	h.start1(&l, 9)
	if _, _, err := h.s.ReportFinished(h.ctx, l.id, l.grant, l.req.Runner, l.proc, exited(0)); err != nil {
		t.Fatal(err)
	}
	if j := h.job(l.id); j.State == pb.JobState_JOB_STATE_FINISHED {
		t.Fatal("an inconclusive probe released the reservation")
	}
}

func TestProbeErrorsAreReportedWithoutChangingNativeOutcome(t *testing.T) {
	probe := &fakeProbe{}
	var warnings []string
	cfg := DefaultConfig(2)
	cfg.Settle = 0
	var h *harness
	callbackUnderLock := false
	h = &harness{
		t: t, ctx: context.Background(), path: filepath.Join(t.TempDir(), "state.db"),
		cfg: cfg, sampler: &fakeSampler{sample: idle}, probe: probe,
		warnf: func(format string, args ...any) {
			if h.s.mu.TryLock() {
				h.s.mu.Unlock()
			} else {
				callbackUnderLock = true
			}
			warnings = append(warnings, fmt.Sprintf(format, args...))
		},
	}
	h.start()
	t.Cleanup(func() { h.store.Close() })

	l := h.granted(2)
	h.start1(&l, 4242)
	descendant := &pb.ProcessIdentity{Pid: 4243, StartTime: contract.Timestamp(5243)}
	if _, err := h.store.Write(h.ctx, func(tx *storage.Tx) error {
		return tx.TrackProcess(l.id, descendant, "descendant")
	}); err != nil {
		t.Fatal(err)
	}
	probe.setError(errors.New("process observation denied"))
	outcome := exited(7)
	j, _, err := h.s.ReportFinished(h.ctx, l.id, l.grant, l.req.Runner, l.proc, outcome)
	if err != nil || !proto.Equal(j.GetOutcome(), outcome) || j.State == pb.JobState_JOB_STATE_FINISHED {
		t.Fatalf("failed probe changed finish report or released capacity: job=%v err=%v", j, err)
	}
	for _, want := range []string{"command", "4242", "descendant", "4243", l.id, "process observation denied"} {
		if !strings.Contains(strings.Join(warnings, "\n"), want) {
			t.Fatalf("probe warning missing %q: %v", want, warnings)
		}
	}
	if len(warnings) != 2 {
		t.Fatalf("expected one warning per tracked process: %v", warnings)
	}
	if callbackUnderLock {
		t.Fatal("probe warning callback ran under the scheduler lock")
	}
	if _, _, err := h.s.ReportFinished(h.ctx, l.id, l.grant, l.req.Runner, l.proc, outcome); err != nil {
		t.Fatalf("duplicate native report after a probe error: %v", err)
	}
	if len(warnings) != 2 {
		t.Fatalf("unchanged errors produced duplicate warnings: %v", warnings)
	}
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 3 || !strings.Contains(warnings[2], "runner") {
		t.Fatalf("runner probe error was not separately reported: %v", warnings)
	}
	if j := h.job(l.id); !proto.Equal(j.GetOutcome(), outcome) || j.State == pb.JobState_JOB_STATE_FINISHED {
		t.Fatalf("runner probe error changed outcome or released capacity: %v", j)
	}

	probe.setError(nil)
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	probe.setError(errors.New("process observation denied"))
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 6 {
		t.Fatalf("recurring errors after recovery were not reported: %v", warnings)
	}
	probe.setError(nil)
	probe.exit(l.proc.Pid)
	probe.exit(descendant.Pid)
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if j := h.expectState(l.id, pb.JobState_JOB_STATE_FINISHED); !proto.Equal(j.GetOutcome(), outcome) {
		t.Fatalf("verified release changed the native outcome: %v", j.Outcome)
	}
}

func TestLaunchFailureReleasesWithNoSpawnProof(t *testing.T) {
	h, _ := launchHarness(t, 2, &fakeProbe{})
	l := h.granted(2)
	_, next, _ := h.queued(2)
	if _, _, _, err := h.s.Claim(h.ctx, l.id, l.grant, l.req.Runner); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.s.ReportLaunchFailed(h.ctx, l.id, l.grant, l.req.Runner, "not found", false); err == nil {
		if j := h.job(l.id); j.State == pb.JobState_JOB_STATE_FINISHED {
			t.Fatal("launch failure released before the no-spawn seal")
		}
	}
	if _, _, err := h.s.Seal(h.ctx, l.id, l.grant, l.req.Runner, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.s.ReportLaunchFailed(h.ctx, l.id, l.grant, l.req.Runner, "not found", false); err != nil {
		t.Fatal(err)
	}
	j := h.expectState(l.id, pb.JobState_JOB_STATE_FINISHED)
	if j.Outcome.GetKind() != pb.OutcomeKind_OUTCOME_KIND_LAUNCH_FAILED {
		t.Fatalf("outcome = %v", j.Outcome)
	}
	h.expectState(next, pb.JobState_JOB_STATE_RESERVED)
}

func TestCancelByState(t *testing.T) {
	h, published := launchHarness(t, 2, &fakeProbe{})
	running := h.granted(2)
	_, queued, _ := h.queued(2)
	h.start1(&running, 5)

	if _, err := h.s.Cancel(h.ctx, queued, proto.String("user")); err != nil {
		t.Fatal(err)
	}
	if j := h.expectState(queued, pb.JobState_JOB_STATE_FINISHED); j.Outcome.GetKind() != pb.OutcomeKind_OUTCOME_KIND_CANCELLED_BEFORE_START {
		t.Fatalf("queued cancel outcome = %v", j.Outcome)
	}

	// A claimed job keeps its reservation until the runner's reports.
	if _, err := h.s.Cancel(h.ctx, running.id, nil); err != nil {
		t.Fatal(err)
	}
	if j := h.job(running.id); j.State == pb.JobState_JOB_STATE_FINISHED || j.CancelRequestedAt == nil {
		t.Fatalf("claimed job after cancel: state %v requested %v", j.State, j.CancelRequestedAt)
	}
	if _, _, err := h.s.AcknowledgeCancel(h.ctx, running.id, running.grant, running.req.Runner); err != nil {
		t.Fatal(err)
	}

	var last int64
	for _, batch := range *published {
		for _, ev := range batch {
			if ev.EventSeq <= last {
				t.Fatalf("published sequence %d after %d", ev.EventSeq, last)
			}
			last = ev.EventSeq
		}
	}
	if last == 0 || last != h.s.EventSeq() {
		t.Fatalf("last published %d, EventSeq %d", last, h.s.EventSeq())
	}
}

func TestCancelFencesUnclaimedGrant(t *testing.T) {
	h, _ := launchHarness(t, 2, &fakeProbe{})
	l := h.granted(2)
	_, next, _ := h.queued(2)
	if _, err := h.s.Cancel(h.ctx, l.id, nil); err != nil {
		t.Fatal(err)
	}
	if j := h.expectState(l.id, pb.JobState_JOB_STATE_FINISHED); j.Outcome.GetKind() != pb.OutcomeKind_OUTCOME_KIND_CANCELLED_BEFORE_START {
		t.Fatalf("outcome = %v", j.Outcome)
	}
	if _, _, _, err := h.s.Claim(h.ctx, l.id, l.grant, l.req.Runner); !errors.Is(err, ErrGrantFenced) {
		t.Fatalf("claim after cancel err = %v, want fenced", err)
	}
	h.expectState(next, pb.JobState_JOB_STATE_RESERVED)
}

func TestClaimRefusedUntilReady(t *testing.T) {
	h, _ := launchHarness(t, 2, &fakeProbe{})
	l := h.granted(2)
	if err := h.s.Stop(h.ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	if h.store, err = storage.Open(h.ctx, h.path); err != nil {
		t.Fatal(err)
	}
	if h.s, _, err = Start(h.ctx, h.store, Options{Config: h.cfg, ServiceVersion: "test", PID: 2, Sampler: h.sampler, Processes: h.probe}); err != nil {
		t.Fatal(err)
	}
	if h.s.Ready() {
		t.Fatal("ready before MarkReady")
	}
	if _, _, _, err := h.s.Claim(h.ctx, l.id, l.grant, l.req.Runner); err == nil {
		t.Fatal("claim accepted while recovering")
	}
	if _, err := h.s.MarkReady(h.ctx); err != nil {
		t.Fatal(err)
	}
	// Ready brokers never relaunch on recovery; the original runner claims.
	if _, already, _, err := h.s.Claim(h.ctx, l.id, l.grant, l.req.Runner); err != nil || already {
		t.Fatalf("claim after recovery: already=%v err=%v", already, err)
	}
}

func TestRecoveryAcceptsLifecycleReportsBeforeAdmission(t *testing.T) {
	h, _ := launchHarness(t, 2, &fakeProbe{})
	l := h.granted(1)
	if _, _, _, err := h.s.Claim(h.ctx, l.id, l.grant, l.req.Runner); err != nil {
		t.Fatal(err)
	}
	if err := h.s.Stop(h.ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	if h.store, err = storage.Open(h.ctx, h.path); err != nil {
		t.Fatal(err)
	}
	h.clock = &fakeClock{}
	if h.s, _, err = Start(h.ctx, h.store, Options{Config: h.cfg, ServiceVersion: "test", PID: 2,
		Clock: h.clock.read, Sampler: h.sampler, Processes: h.probe}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.s.Submit(h.ctx, request(t, 1)); !errors.Is(err, ErrNotReady) {
		t.Fatalf("submission during recovery = %v", err)
	}
	if _, _, _, err := h.s.Claim(h.ctx, l.id, l.grant, l.req.Runner); !errors.Is(err, ErrNotReady) {
		t.Fatalf("claim during recovery = %v", err)
	}
	if _, err := h.s.Rejoin(h.ctx, l.id, l.req.Runner); err != nil {
		t.Fatalf("rejoin during recovery: %v", err)
	}
	if _, _, err := h.s.Seal(h.ctx, l.id, l.grant, l.req.Runner, false); err != nil {
		t.Fatalf("seal during recovery: %v", err)
	}
	root := &pb.ProcessIdentity{Pid: 4242, StartTime: contract.Timestamp(5242)}
	if _, _, err := h.s.ReportFinished(h.ctx, l.id, l.grant, l.req.Runner, root, exited(1)); err != nil {
		t.Fatalf("finish during recovery: %v", err)
	}
	if _, err := h.s.MarkReady(h.ctx); err != nil {
		t.Fatal(err)
	}
	_, other, _ := h.queued(1)
	h.expectState(other, pb.JobState_JOB_STATE_RESERVED)
	if occupied := h.snapshot().Status.Occupied; occupied != 2 {
		t.Fatalf("recovered reservation not counted: %d", occupied)
	}
}

func TestRecoveredUncertainReservationsRemainVisibleAndCounted(t *testing.T) {
	h, _ := launchHarness(t, 2, &fakeProbe{})
	l := h.granted(1)
	h.restart(2)
	if status := h.snapshot().Status; status.UnreconciledReservations != 1 || status.Occupied != 1 {
		t.Fatalf("recovered status = %v", status)
	}
	_, other, _ := h.queued(1)
	h.expectState(other, pb.JobState_JOB_STATE_RESERVED)
	if status := h.snapshot().Status; status.UnreconciledReservations != 1 || status.Occupied != 2 {
		t.Fatalf("spare-capacity status = %v", status)
	}
	if _, err := h.s.Cancel(h.ctx, l.id, nil); err != nil {
		t.Fatal(err)
	}
	if status := h.snapshot().Status; status.UnreconciledReservations != 0 || status.Occupied != 1 {
		t.Fatalf("released recovery status = %v", status)
	}
}

func TestUnreconciledCountChangesArePublished(t *testing.T) {
	for _, scenario := range []string{"launch timeout", "cancellation", "finish", "launch failure"} {
		t.Run(scenario, func(t *testing.T) {
			probe := &fakeProbe{}
			h, _ := launchHarness(t, 2, probe)
			l := h.granted(2)
			var events []*pb.BrokerEvent
			var err error
			switch scenario {
			case "launch timeout":
				h.clock.advance(30 * time.Second)
				events, err = h.s.Reconcile(h.ctx)
			case "cancellation":
				h.start1(&l, 4242)
				events, err = h.s.Cancel(h.ctx, l.id, nil)
			case "finish":
				h.start1(&l, 4242)
				_, events, err = h.s.ReportFinished(h.ctx, l.id, l.grant, l.req.Runner, l.proc, exited(1))
			case "launch failure":
				if _, _, _, err := h.s.Claim(h.ctx, l.id, l.grant, l.req.Runner); err != nil {
					t.Fatal(err)
				}
				_, events, err = h.s.ReportLaunchFailed(h.ctx, l.id, l.grant, l.req.Runner, "start failed", false)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if status := event.GetStatusChanged(); status != nil && status.UnreconciledReservations == 1 {
					return
				}
			}
			t.Fatalf("no status event published the unreconciled reservation: %v", events)
		})
	}
}

func TestStorageFailureClosesAdmissionUntilReopen(t *testing.T) {
	h, _ := launchHarness(t, 2, &fakeProbe{})
	l := h.granted(2)
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Reconcile(h.ctx); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("storage failure = %v", err)
	}
	if h.s.Ready() {
		t.Fatal("scheduler remained ready after storage failure")
	}
	if _, _, _, err := h.s.Claim(h.ctx, l.id, l.grant, l.req.Runner); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("claim after storage failure = %v", err)
	}

	var err error
	if h.store, err = storage.Open(h.ctx, h.path); err != nil {
		t.Fatal(err)
	}
	h.clock = &fakeClock{}
	if h.s, _, err = Start(h.ctx, h.store, Options{Config: h.cfg, ServiceVersion: "test", PID: 2,
		Clock: h.clock.read, Sampler: h.sampler, Processes: h.probe}); err != nil {
		t.Fatal(err)
	}
	if h.s.Ready() {
		t.Fatal("reopened storage skipped recovery")
	}
	if _, err := h.s.MarkReady(h.ctx); err != nil {
		t.Fatal(err)
	}
	if occupied := h.snapshot().Status.Occupied; occupied != 2 {
		t.Fatalf("reopen lost the held reservation: %d", occupied)
	}
}

func TestSnapshotsProjectLaunchTimeout(t *testing.T) {
	h := newHarness(t, 2, nil)
	req := request(t, 2)
	_, submitted, _, err := h.s.TrySubmitAndWatch(h.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	_, watched, _, err := h.s.Watch(h.ctx, submitted.JobId, req.Runner)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := h.snapshot()
	if len(snapshot.ActiveJobs) != 1 {
		t.Fatalf("active jobs = %d, want 1", len(snapshot.ActiveJobs))
	}
	for name, job := range map[string]*pb.Job{
		"no-wait snapshot": submitted,
		"watch snapshot":   watched,
		"broker snapshot":  snapshot.ActiveJobs[0],
	} {
		t.Run(name, func(t *testing.T) {
			if job.Grant == nil || job.Grant.LaunchTimeout == nil || job.Grant.LaunchTimeout.AsDuration().Milliseconds() != 30000 {
				t.Fatalf("grant = %v, want the 30-second launch investigation timeout", job.Grant)
			}
		})
	}
}

func TestLaunchInvestigationStartsAtReservationAndRetainsCapacity(t *testing.T) {
	h := newHarness(t, 2, nil)
	l := h.granted(1)

	h.clock.advance(20 * time.Second)
	if _, _, _, err := h.s.Claim(h.ctx, l.id, l.grant, l.req.Runner); err != nil {
		t.Fatal(err)
	}
	h.watch(l.req, l.id)
	h.clock.advance(9 * time.Second)
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if j := h.job(l.id); j.Outcome != nil {
		t.Fatalf("investigation began before the reservation deadline: %v", j.Outcome)
	}

	h.clock.advance(time.Second)
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	j := h.expectState(l.id, pb.JobState_JOB_STATE_RESERVED)
	if j.Outcome.GetKind() != pb.OutcomeKind_OUTCOME_KIND_UNKNOWN || j.Outcome.Detail == nil {
		t.Fatalf("expired launch investigation not visible: %v", j.Outcome)
	}
	if occupied := h.snapshot().Status.Occupied; occupied != 1 {
		t.Fatalf("investigation changed occupied capacity: %d", occupied)
	}
	_, other, _ := h.queued(1)
	h.expectState(other, pb.JobState_JOB_STATE_RESERVED)
	if occupied := h.snapshot().Status.Occupied; occupied != 2 {
		t.Fatalf("uncertain reservation was not counted: %d", occupied)
	}
}

func TestLaunchInvestigationRestartsAfterBrokerRecovery(t *testing.T) {
	h := newHarness(t, 2, nil)
	l := h.granted(2)
	h.clock.advance(20 * time.Second)
	h.restart(2)

	h.clock.advance(29 * time.Second)
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if j := h.job(l.id); j.Outcome != nil {
		t.Fatalf("broker restart charged the old clock or downtime: %v", j.Outcome)
	}
	h.clock.advance(time.Second)
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if j := h.expectState(l.id, pb.JobState_JOB_STATE_RESERVED); j.Outcome.GetKind() != pb.OutcomeKind_OUTCOME_KIND_UNKNOWN {
		t.Fatalf("reconstructed investigation did not expire: %v", j.Outcome)
	}
}

func TestConfiguredLaunchInvestigationForEachAdmissionMode(t *testing.T) {
	for _, mode := range []string{"queued", "no-wait", "forced"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t, 2, func(c *Config) { c.LaunchTimeout = 7 * time.Second })
			var j *pb.Job
			switch mode {
			case "queued":
				l := h.granted(1)
				j = h.job(l.id)
			case "no-wait":
				var err error
				_, j, _, err = h.s.TrySubmitAndWatch(h.ctx, request(t, 1))
				if err != nil {
					t.Fatal(err)
				}
			case "forced":
				if _, err := h.s.SetQueueMode(h.ctx, pb.QueueMode_QUEUE_MODE_PAUSED); err != nil {
					t.Fatal(err)
				}
				_, id, _ := h.queued(1)
				var err error
				j, _, err = h.s.ForceStart(h.ctx, id)
				if err != nil {
					t.Fatal(err)
				}
			}
			if j.Grant.GetLaunchTimeout().AsDuration() != 7*time.Second {
				t.Fatalf("advertised launch timeout = %v", j.Grant.GetLaunchTimeout().AsDuration())
			}
			h.clock.advance(7 * time.Second)
			if _, err := h.s.Reconcile(h.ctx); err != nil {
				t.Fatal(err)
			}
			j = h.expectState(j.JobId, pb.JobState_JOB_STATE_RESERVED)
			if j.Outcome.GetKind() != pb.OutcomeKind_OUTCOME_KIND_UNKNOWN {
				t.Fatalf("configured deadline did not trigger investigation: %v", j.Outcome)
			}
			if _, _, _, err := h.s.Claim(h.ctx, j.JobId, j.Grant.GrantId, j.Grant.Runner); err != nil {
				t.Fatalf("timeout incorrectly fenced the grant: %v", err)
			}
		})
	}
}

func TestRunnerDeathFencesUnknownRootIndependentlyOfDisconnect(t *testing.T) {
	probe := &fakeProbe{}
	h, _ := launchHarness(t, 2, probe)
	l := h.granted(1)
	if _, _, _, err := h.s.Claim(h.ctx, l.id, l.grant, l.req.Runner); err != nil {
		t.Fatal(err)
	}
	reg := h.watch(l.req, l.id)
	if _, err := h.s.Disconnect(h.ctx, reg); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if j := h.job(l.id); j.BrokerTermination != nil {
		t.Fatalf("disconnect triggered takeover: %v", j.BrokerTermination)
	}

	probe.exit(l.req.Runner.Process.Pid)
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	j := h.expectState(l.id, pb.JobState_JOB_STATE_RESERVED)
	if j.Outcome.GetKind() != pb.OutcomeKind_OUTCOME_KIND_UNKNOWN ||
		j.BrokerTermination.GetReason() != pb.BrokerTermination_REASON_RUNNER_DEAD {
		t.Fatalf("verified runner death not surfaced: outcome=%v termination=%v", j.Outcome, j.BrokerTermination)
	}
	if err := h.store.View(h.ctx, func(r *storage.Reader) error {
		res, err := r.Reservation(l.id)
		if err == nil && (res.FenceReason == nil || *res.FenceReason != "runner_terminated" || res.ReleasedAt != nil) {
			t.Fatalf("unknown-root reservation = %+v", res)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, other, _ := h.queued(1)
	h.expectState(other, pb.JobState_JOB_STATE_RESERVED)
	if occupied := h.snapshot().Status.Occupied; occupied != 2 {
		t.Fatalf("unknown-root reservation was not counted: %d", occupied)
	}
}

func TestRunnerDeathClosesAnUnclaimedGrantWithoutLaunching(t *testing.T) {
	probe := &fakeProbe{}
	h, _ := launchHarness(t, 2, probe)
	l := h.granted(2)
	probe.exit(l.req.Runner.Process.Pid)
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	j := h.expectState(l.id, pb.JobState_JOB_STATE_FINISHED)
	if j.Outcome.GetKind() != pb.OutcomeKind_OUTCOME_KIND_UNKNOWN {
		t.Fatalf("outcome = %v", j.Outcome)
	}
	if err := h.store.View(h.ctx, func(r *storage.Reader) error {
		res, err := r.Reservation(l.id)
		if err == nil && (res.ClaimedAt != nil || res.NoLaunchProof == nil ||
			*res.NoLaunchProof != "claim_prevented" || res.ReleasedAt == nil) {
			t.Fatalf("unclaimed grant was not atomically closed: %+v", res)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := h.s.Claim(h.ctx, l.id, l.grant, l.req.Runner); !errors.Is(err, ErrGrantFenced) {
		t.Fatalf("closed grant claim = %v", err)
	}
}

func TestRunnerDeathReleaseRequiresRootAndKnownDescendantExit(t *testing.T) {
	probe := &fakeProbe{}
	h, _ := launchHarness(t, 2, probe)
	l := h.granted(2)
	h.start1(&l, 4242)
	descendant := &pb.ProcessIdentity{Pid: 4243, StartTime: contract.Timestamp(5243)}
	if _, err := h.store.Write(h.ctx, func(tx *storage.Tx) error {
		return tx.TrackProcess(l.id, descendant, "descendant")
	}); err != nil {
		t.Fatal(err)
	}
	probe.exit(l.req.Runner.Process.Pid)
	probe.exit(l.proc.Pid)
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if j := h.job(l.id); j.State == pb.JobState_JOB_STATE_FINISHED ||
		j.Outcome.GetKind() != pb.OutcomeKind_OUTCOME_KIND_UNKNOWN ||
		j.BrokerTermination.GetReason() != pb.BrokerTermination_REASON_RUNNER_DEAD {
		t.Fatalf("known live descendant was not retained: %v", j)
	}
	probe.exit(descendant.Pid)
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	j := h.expectState(l.id, pb.JobState_JOB_STATE_FINISHED)
	if j.Outcome.GetKind() != pb.OutcomeKind_OUTCOME_KIND_UNKNOWN || j.Outcome.ExitCode != nil {
		t.Fatalf("recovery invented a native outcome: %v", j.Outcome)
	}
	late, _, err := h.s.ReportFinished(h.ctx, l.id, l.grant, l.req.Runner, l.proc, exited(3))
	if err != nil || late.State != pb.JobState_JOB_STATE_FINISHED || late.Outcome.GetExitCode() != 3 ||
		late.BrokerTermination.GetReason() != pb.BrokerTermination_REASON_RUNNER_DEAD {
		t.Fatalf("late report lost recovery evidence: job=%v err=%v", late, err)
	}
}

func TestUnverifiableRunnerIsVisibleAndHeld(t *testing.T) {
	probe := &fakeProbe{err: errors.New("process observation denied")}
	h, _ := launchHarness(t, 2, probe)
	l := h.granted(2)
	if _, _, _, err := h.s.Claim(h.ctx, l.id, l.grant, l.req.Runner); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	j := h.expectState(l.id, pb.JobState_JOB_STATE_RESERVED)
	if j.BrokerTermination != nil || j.Outcome.GetKind() != pb.OutcomeKind_OUTCOME_KIND_UNKNOWN ||
		!strings.Contains(j.Outcome.GetDetail(), "unverifiable") {
		t.Fatalf("unverifiable runner not surfaced safely: %v", j)
	}
}

func TestLaunchInvestigationStopsWhenRootIsReported(t *testing.T) {
	h, _ := launchHarness(t, 2, &fakeProbe{})
	l := h.granted(2)
	h.start1(&l, 4242)
	h.clock.advance(LaunchTimeout)
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if j := h.job(l.id); j.Outcome != nil {
		t.Fatalf("reported root was treated as an unresolved launch: %v", j.Outcome)
	}
}

func TestSubmitProjectsDisconnectedGrace(t *testing.T) {
	h := newHarness(t, 2, nil)
	result, _, err := h.s.Submit(h.ctx, request(t, 2))
	if err != nil {
		t.Fatal(err)
	}
	if result.Job.GraceRemaining == nil || result.Job.GraceRemaining.AsDuration().Milliseconds() != 30000 {
		t.Fatalf("submitted job grace = %v, want 30 seconds", result.Job.GraceRemaining)
	}
}
