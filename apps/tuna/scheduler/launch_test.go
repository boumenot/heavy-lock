package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

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
	l.proc = &pb.ProcessIdentity{Pid: pid, StartTimeUnixNs: 1000 + pid}
	if _, _, err := h.s.ReportStarted(h.ctx, l.id, l.grant, l.req.Runner, l.proc); err != nil {
		h.t.Fatalf("started: %v", err)
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

func TestMissingProbeFailsClosed(t *testing.T) {
	h, _ := launchHarness(t, 2, nil)
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
	if j := h.job(running.id); j.State == pb.JobState_JOB_STATE_FINISHED || j.CancelRequestedAtUnixNs == nil {
		t.Fatalf("claimed job after cancel: state %v requested %v", j.State, j.CancelRequestedAtUnixNs)
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
			if job.Grant == nil || job.Grant.LaunchTimeoutMs != 30000 {
				t.Fatalf("grant = %v, want the 30-second launch investigation timeout", job.Grant)
			}
		})
	}
}

func TestSubmitProjectsDisconnectedGrace(t *testing.T) {
	h := newHarness(t, 2, nil)
	result, _, err := h.s.Submit(h.ctx, request(t, 2))
	if err != nil {
		t.Fatal(err)
	}
	if result.Job.GraceRemainingMs == nil || *result.Job.GraceRemainingMs != 30000 {
		t.Fatalf("submitted job grace = %v, want 30 seconds", result.Job.GraceRemainingMs)
	}
}
