package scheduler

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"github.com/boumenot/heavy-lock/apps/tuna/storage"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Duration
}

func (c *fakeClock) read() time.Duration     { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) advance(d time.Duration) { c.mu.Lock(); c.now += d; c.mu.Unlock() }

type fakeSampler struct {
	mu     sync.Mutex
	sample Sample
	err    error
	calls  int
	during func()
}

func (f *fakeSampler) Sample(ctx context.Context) (Sample, error) {
	f.mu.Lock()
	during := f.during
	f.calls++
	sample, err := f.sample, f.err
	f.mu.Unlock()
	if during != nil {
		during()
	}
	if ctx.Err() != nil {
		return Sample{}, ctx.Err()
	}
	return sample, err
}

func (f *fakeSampler) set(s Sample, err error) { f.mu.Lock(); f.sample, f.err = s, err; f.mu.Unlock() }

var idle = Sample{TotalMemoryBytes: 1000, AvailableMemoryBytes: 800, LoadAverage1m: 0.5, CPULoadPercent: 5}

type harness struct {
	t       *testing.T
	ctx     context.Context
	path    string
	cfg     Config
	store   *storage.Store
	clock   *fakeClock
	sampler *fakeSampler
	probe   ProcessProbe
	publish func([]*pb.BrokerEvent)
	s       *Scheduler
}

func newHarness(t *testing.T, capacity uint32, adjust func(*Config)) *harness {
	t.Helper()
	cfg := DefaultConfig(capacity)
	cfg.Settle = 0
	if adjust != nil {
		adjust(&cfg)
	}
	h := &harness{t: t, ctx: context.Background(), path: filepath.Join(t.TempDir(), "state.db"), cfg: cfg,
		sampler: &fakeSampler{sample: idle}}
	h.start()
	t.Cleanup(func() { h.store.Close() })
	return h
}

func (h *harness) start() {
	h.t.Helper()
	var err error
	if h.store, err = storage.Open(h.ctx, h.path); err != nil {
		h.t.Fatal(err)
	}
	h.clock = &fakeClock{}
	if h.s, _, err = Start(h.ctx, h.store, Options{Config: h.cfg, ServiceVersion: "test", PID: 1, Clock: h.clock.read, Sampler: h.sampler,
		Processes: h.probe, Publish: h.publish}); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.s.MarkReady(h.ctx); err != nil {
		h.t.Fatal(err)
	}
}

// restart stops cleanly and starts a new run with a fresh monotonic clock,
// so broker downtime is never measured.
func (h *harness) restart(capacity uint32) {
	h.t.Helper()
	if err := h.s.Stop(h.ctx); err != nil {
		h.t.Fatal(err)
	}
	if err := h.store.Close(); err != nil {
		h.t.Fatal(err)
	}
	h.cfg.Capacity = capacity
	h.start()
}

func uuid4(t testing.TB) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func request(t testing.TB, weight uint32) *pb.SubmitJobRequest {
	return &pb.SubmitJobRequest{IdempotencyKey: uuid4(t), Command: "example", Weight: weight,
		Runner: &pb.RunnerIncarnation{IncarnationId: uuid4(t), Process: &pb.ProcessIdentity{Pid: 1, StartTimeUnixNs: 1}}}
}

func (h *harness) submit(weight uint32) (*pb.SubmitJobRequest, string) {
	h.t.Helper()
	req := request(h.t, weight)
	result, _, err := h.s.Submit(h.ctx, req)
	if err != nil || !result.Created {
		h.t.Fatalf("submit: %v %+v", err, result)
	}
	return req, result.Job.JobId
}

func (h *harness) watch(req *pb.SubmitJobRequest, id string) *Registration {
	h.t.Helper()
	reg, _, _, err := h.s.Watch(h.ctx, id, req.Runner)
	if err != nil || reg == nil {
		h.t.Fatalf("watch %s: %v", id, err)
	}
	return reg
}

func (h *harness) queued(weight uint32) (*pb.SubmitJobRequest, string, *Registration) {
	req, id := h.submit(weight)
	return req, id, h.watch(req, id)
}

func (h *harness) job(id string) *pb.Job {
	h.t.Helper()
	j, err := h.s.Job(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return j
}

func (h *harness) snapshot() *pb.BrokerSnapshot {
	h.t.Helper()
	snap, err := h.s.Snapshot(h.ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	return snap
}

func (h *harness) expectState(id string, want pb.JobState) *pb.Job {
	h.t.Helper()
	j := h.job(id)
	if j.State != want {
		h.t.Fatalf("job %s state %v, want %v", id, j.State, want)
	}
	return j
}

// cancel releases an unclaimed reservation (or removes a queued job), as
// lifecycle code outside the scheduler would, then reschedules.
func (h *harness) cancel(id string) {
	h.t.Helper()
	if _, err := h.store.Write(h.ctx, func(tx *storage.Tx) error { return tx.Cancel(id, nil) }); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.s.Schedule(h.ctx); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) tick(d time.Duration) {
	h.t.Helper()
	h.clock.advance(d)
	if _, err := h.s.Tick(h.ctx); err != nil {
		h.t.Fatal(err)
	}
}

func reserved(j *pb.Job) bool { return j.State == pb.JobState_JOB_STATE_RESERVED }

func TestWeightedFIFOWithBoundedBackfill(t *testing.T) {
	h := newHarness(t, 4, nil)
	_, a, _ := h.queued(3)
	h.expectState(a, pb.JobState_JOB_STATE_RESERVED)
	_, b, _ := h.queued(3)
	_, c, _ := h.queued(1)
	_, d, _ := h.queued(1)
	if j := h.expectState(c, pb.JobState_JOB_STATE_RESERVED); !j.AdmittedByBackfill {
		t.Fatal("follower was not admitted by backfill")
	}
	h.expectState(d, pb.JobState_JOB_STATE_QUEUED)
	if j := h.job(b); j.BackfillBypassCount != 1 || j.State != pb.JobState_JOB_STATE_QUEUED {
		t.Fatalf("head = %v bypass %d", j.State, j.BackfillBypassCount)
	}
	snap := h.snapshot()
	if snap.Status.Occupied != 4 || snap.Status.Capacity != 4 || snap.Status.BackfillLimit != 1 ||
		snap.Status.HeadBlockedReason != pb.BlockedReason_BLOCKED_REASON_INSUFFICIENT_SLOTS ||
		snap.Queue.HeadBlockedReason != pb.BlockedReason_BLOCKED_REASON_INSUFFICIENT_SLOTS {
		t.Fatalf("status %+v queue reason %v", snap.Status, snap.Queue.HeadBlockedReason)
	}
	h.cancel(a)
	h.expectState(b, pb.JobState_JOB_STATE_RESERVED)
	if j := h.expectState(d, pb.JobState_JOB_STATE_QUEUED); j.QueuePosition == nil || *j.QueuePosition != 0 {
		t.Fatal("later job did not become head")
	}
}

func TestDisconnectedHeadBlocksAndUnregisteredFollowerKeepsPlace(t *testing.T) {
	h := newHarness(t, 4, nil)
	reqHead, head := h.submit(1)
	_, follower, _ := h.queued(1)
	h.expectState(follower, pb.JobState_JOB_STATE_QUEUED)
	if r := h.snapshot().Status.HeadBlockedReason; r != pb.BlockedReason_BLOCKED_REASON_RUNNER_DISCONNECTED {
		t.Fatalf("head reason %v", r)
	}
	h.watch(reqHead, head)
	h.expectState(head, pb.JobState_JOB_STATE_RESERVED)
	h.expectState(follower, pb.JobState_JOB_STATE_RESERVED)

	h2 := newHarness(t, 4, nil)
	h2.queued(3)
	_, blocked, _ := h2.queued(2)
	_, unregistered := h2.submit(1)
	_, later, _ := h2.queued(1)
	h2.expectState(later, pb.JobState_JOB_STATE_RESERVED)
	if j := h2.expectState(unregistered, pb.JobState_JOB_STATE_QUEUED); *j.QueuePosition != 1 || j.GraceRemainingMs == nil {
		t.Fatalf("unregistered follower = %+v", j)
	}
	h2.expectState(blocked, pb.JobState_JOB_STATE_QUEUED)
}

func TestSharingChecksSettleAndRecordFreeMemory(t *testing.T) {
	h := newHarness(t, 4, func(c *Config) { c.Settle = time.Minute })
	_, alone, _ := h.queued(1)
	if j := h.expectState(alone, pb.JobState_JOB_STATE_RESERVED); j.FreeMemoryAtAdmissionBytes == nil || *j.FreeMemoryAtAdmissionBytes != 800 {
		t.Fatalf("free memory at admission = %v", j.FreeMemoryAtAdmissionBytes)
	}
	calls := h.sampler.calls
	_, next, _ := h.queued(1)
	h.expectState(next, pb.JobState_JOB_STATE_QUEUED)
	if r := h.snapshot().Status.HeadBlockedReason; r != pb.BlockedReason_BLOCKED_REASON_RESOURCE_CHECK {
		t.Fatalf("settling head reason %v", r)
	}
	if h.sampler.calls != calls {
		t.Fatal("sampled resources while settling")
	}
	h.sampler.set(Sample{TotalMemoryBytes: 1000, AvailableMemoryBytes: 100, CPULoadPercent: 5}, nil)
	h.tick(time.Minute)
	h.expectState(next, pb.JobState_JOB_STATE_QUEUED)
	if s := h.snapshot().Status; s.FreeMemoryBytes == nil || *s.FreeMemoryBytes != 100 {
		t.Fatalf("status sample %+v", s)
	}
	h.sampler.set(Sample{}, errors.New("sysctl failed"))
	h.tick(time.Second)
	h.expectState(next, pb.JobState_JOB_STATE_QUEUED)
	h.sampler.set(idle, nil)
	h.tick(time.Second)
	h.expectState(next, pb.JobState_JOB_STATE_RESERVED)
}

func TestPausePersistsAndForceStartBypassesPauseAndCapacity(t *testing.T) {
	h := newHarness(t, 2, nil)
	if _, err := h.s.SetQueueMode(h.ctx, pb.QueueMode_QUEUE_MODE_PAUSED); err != nil {
		t.Fatal(err)
	}
	reqA, a, _ := h.queued(2)
	h.expectState(a, pb.JobState_JOB_STATE_QUEUED)
	h.restart(2)
	snap := h.snapshot()
	if snap.Status.QueueMode != pb.QueueMode_QUEUE_MODE_PAUSED || !snap.Status.Paused ||
		snap.Status.QueueState != pb.QueueState_QUEUE_STATE_PAUSED || snap.Queue.QueueState != pb.QueueState_QUEUE_STATE_PAUSED ||
		snap.Status.HeadBlockedReason != pb.BlockedReason_BLOCKED_REASON_PAUSED {
		t.Fatalf("after restart: %+v", snap.Status)
	}
	if j := h.job(a); j.RunnerConnected || j.GraceRemainingMs == nil || *j.GraceRemainingMs != 30000 {
		t.Fatalf("connected job not disconnected with fresh grace: %+v", j)
	}
	if _, _, err := h.s.ForceStart(h.ctx, a); !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("force-start of unregistered job: %v", err)
	}
	h.watch(reqA, a)
	if _, _, err := h.s.ForceStart(h.ctx, a); err != nil {
		t.Fatal(err)
	}
	_, b, _ := h.queued(2)
	j, _, err := h.s.ForceStart(h.ctx, b)
	if err != nil || !j.Forced || j.AdmittedByBackfill {
		t.Fatalf("force-start: %v %+v", err, j)
	}
	if s := h.snapshot().Status; s.Occupied != 4 || s.Capacity != 2 {
		t.Fatalf("forced occupancy %+v", s)
	}
	if _, _, err := h.s.ForceStart(h.ctx, b); !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("second force-start: %v", err)
	}
	if _, err := h.s.SetQueueMode(h.ctx, pb.QueueMode_QUEUE_MODE_ENABLED); err != nil {
		t.Fatal(err)
	}
	_, c, _ := h.queued(1)
	h.expectState(c, pb.JobState_JOB_STATE_QUEUED)
	if st := h.snapshot().Status; st.HeadBlockedReason != pb.BlockedReason_BLOCKED_REASON_INSUFFICIENT_SLOTS ||
		st.QueueMode != pb.QueueMode_QUEUE_MODE_ENABLED || st.QueueState != pb.QueueState_QUEUE_STATE_BLOCKED {
		t.Fatalf("after resume: %+v", st)
	}
	h.cancel(a)
	h.expectState(c, pb.JobState_JOB_STATE_QUEUED)
	h.cancel(b)
	h.expectState(c, pb.JobState_JOB_STATE_RESERVED)
}

func TestReorderAdmitsAndNeverTransfersBypass(t *testing.T) {
	h := newHarness(t, 4, nil)
	_, x := h.submit(3)
	_, a, _ := h.queued(3)
	_, b, _ := h.queued(1)
	_, c, _ := h.queued(1)
	h.expectState(a, pb.JobState_JOB_STATE_QUEUED)
	if r := h.snapshot().Status.HeadBlockedReason; r != pb.BlockedReason_BLOCKED_REASON_RUNNER_DISCONNECTED {
		t.Fatalf("head reason %v", r)
	}
	if _, err := h.s.MoveBefore(h.ctx, x, ""); err != nil {
		t.Fatal(err)
	}
	h.expectState(a, pb.JobState_JOB_STATE_RESERVED)
	h.expectState(b, pb.JobState_JOB_STATE_RESERVED)
	if j := h.expectState(c, pb.JobState_JOB_STATE_QUEUED); *j.QueuePosition != 0 {
		t.Fatalf("c position %d", *j.QueuePosition)
	}
	if _, err := h.s.MoveBefore(h.ctx, a, c); !errors.Is(err, ErrNotWaiting) {
		t.Fatalf("moving a reserved job: %v", err)
	}

	h2 := newHarness(t, 4, nil)
	h2.queued(3)
	_, head, _ := h2.queued(2)
	_, backfilled, _ := h2.queued(1)
	_, later, _ := h2.queued(1)
	h2.expectState(backfilled, pb.JobState_JOB_STATE_RESERVED)
	if _, err := h2.s.MoveBefore(h2.ctx, head, ""); err != nil {
		t.Fatal(err)
	}
	if j := h2.job(head); j.BackfillBypassCount != 1 || *j.QueuePosition != 1 {
		t.Fatalf("moved head %+v", j)
	}
	if j := h2.job(later); j.BackfillBypassCount != 0 || *j.QueuePosition != 0 {
		t.Fatalf("new head %+v", j)
	}
	if _, err := h2.s.MoveToFront(h2.ctx, head); err != nil {
		t.Fatal(err)
	}
	if j := h2.job(head); *j.QueuePosition != 0 {
		t.Fatalf("move to front %+v", j)
	}
}

func TestOversizedQueuedJobAfterCapacityDecrease(t *testing.T) {
	h := newHarness(t, 4, nil)
	_, occupier, _ := h.queued(4)
	reqBig, big, _ := h.queued(3)
	reqSmall, small, _ := h.queued(1)
	h.restart(2)
	if j := h.job(big); j.BlockedReason != pb.BlockedReason_BLOCKED_REASON_WEIGHT_EXCEEDS_CAPACITY {
		t.Fatalf("blocked reason %v", j.BlockedReason)
	}
	if _, _, err := h.s.Submit(h.ctx, request(t, 3)); !errors.Is(err, ErrWeightExceedsCapacity) {
		t.Fatalf("fresh oversized submit: %v", err)
	}
	if result, _, err := h.s.Submit(h.ctx, reqBig); err != nil || result.Created || result.Job.JobId != big {
		t.Fatalf("duplicate of oversized job: %v %+v", err, result)
	}
	h.watch(reqBig, big)
	h.watch(reqSmall, small)
	snap := h.snapshot()
	if snap.Queue.Jobs[0].JobId != big || snap.Status.HeadBlockedReason != pb.BlockedReason_BLOCKED_REASON_INSUFFICIENT_SLOTS {
		t.Fatalf("queue %v reason %v", snap.Queue.Jobs[0].JobId, snap.Status.HeadBlockedReason)
	}
	h.cancel(occupier)
	h.expectState(small, pb.JobState_JOB_STATE_RESERVED)
	h.expectState(big, pb.JobState_JOB_STATE_QUEUED)
	if j, _, err := h.s.ForceStart(h.ctx, big); err != nil || !reserved(j) {
		t.Fatalf("force-start oversized: %v", err)
	}
}

func TestGraceExpiryReconnectAndDowntime(t *testing.T) {
	h := newHarness(t, 1, nil)
	if _, err := h.s.SetQueueMode(h.ctx, pb.QueueMode_QUEUE_MODE_PAUSED); err != nil {
		t.Fatal(err)
	}
	_, unregistered := h.submit(1)
	h.tick(20 * time.Second)
	h.restart(1)
	h.tick(9 * time.Second)
	if j := h.expectState(unregistered, pb.JobState_JOB_STATE_QUEUED); *j.GraceRemainingMs != 1000 {
		t.Fatalf("grace remaining %d", *j.GraceRemainingMs)
	}
	h.tick(time.Second)
	if j := h.expectState(unregistered, pb.JobState_JOB_STATE_FINISHED); j.Outcome.Kind != pb.OutcomeKind_OUTCOME_KIND_REMOVED_DISCONNECTED {
		t.Fatalf("outcome %v", j.Outcome)
	}

	req, id, reg := h.queued(1)
	if _, err := h.s.Disconnect(h.ctx, reg); err != nil {
		t.Fatal(err)
	}
	h.tick(25 * time.Second)
	reg = h.watch(req, id)
	h.tick(time.Hour)
	if _, err := h.s.Disconnect(h.ctx, reg); err != nil {
		t.Fatal(err)
	}
	h.tick(25 * time.Second)
	h.expectState(id, pb.JobState_JOB_STATE_QUEUED)
	h.tick(5 * time.Second)
	h.expectState(id, pb.JobState_JOB_STATE_FINISHED)
	if reg, j, _, err := h.s.Watch(h.ctx, id, req.Runner); err != nil || reg != nil || j.State != pb.JobState_JOB_STATE_FINISHED {
		t.Fatalf("expired job revived: %v %v", reg, err)
	}

	dup, dupID := h.submit(1)
	h.tick(20 * time.Second)
	if result, _, err := h.s.Submit(h.ctx, dup); err != nil || result.Created {
		t.Fatal(err)
	}
	h.tick(10 * time.Second)
	h.expectState(dupID, pb.JobState_JOB_STATE_FINISHED)
}

func TestLatestRegistrationIsAuthoritative(t *testing.T) {
	h := newHarness(t, 1, nil)
	if _, err := h.s.SetQueueMode(h.ctx, pb.QueueMode_QUEUE_MODE_PAUSED); err != nil {
		t.Fatal(err)
	}
	req, id, old := h.queued(1)
	current := h.watch(req, id)
	if _, err := h.s.Disconnect(h.ctx, old); err != nil {
		t.Fatal(err)
	}
	if !h.job(id).RunnerConnected || h.s.Current(old) || !h.s.Current(current) {
		t.Fatal("superseded disconnect unregistered the current stream")
	}
	other := &pb.RunnerIncarnation{IncarnationId: uuid4(t), Process: &pb.ProcessIdentity{Pid: 2, StartTimeUnixNs: 2}}
	if _, _, _, err := h.s.Watch(h.ctx, id, other); !errors.Is(err, storage.ErrIdentity) || !h.s.Current(current) {
		t.Fatalf("other incarnation watch: %v", err)
	}
	streamDone, cancel := context.WithCancel(h.ctx)
	cancel()
	if _, err := h.s.Disconnect(streamDone, current); err != nil || h.job(id).RunnerConnected {
		t.Fatalf("current disconnect with the closed stream context not recorded: %v", err)
	}
}

func TestTickDisconnectsDurablyConnectedJobWithoutStream(t *testing.T) {
	h := newHarness(t, 1, nil)
	if _, err := h.s.SetQueueMode(h.ctx, pb.QueueMode_QUEUE_MODE_PAUSED); err != nil {
		t.Fatal(err)
	}
	req, id := h.submit(1)
	// Simulate a Disconnect whose commit failed: durable state still says
	// connected but the stream is gone.
	if _, err := h.store.Write(h.ctx, func(tx *storage.Tx) error {
		return tx.SetQueueConnection(id, req.Runner, true, h.s.ServiceRunID())
	}); err != nil {
		t.Fatal(err)
	}
	h.tick(time.Second)
	if j := h.job(id); j.RunnerConnected || *j.GraceRemainingMs != 30000 {
		t.Fatalf("stale connection not reconciled: %+v", j)
	}
	if deadline, ok := h.s.NextGraceDeadline(); !ok || deadline != h.clock.read()+30*time.Second {
		t.Fatalf("deadline %v %v", deadline, ok)
	}
	h.tick(30 * time.Second)
	h.expectState(id, pb.JobState_JOB_STATE_FINISHED)
}

func TestNoWaitAdmissionBusyAndBackfill(t *testing.T) {
	h := newHarness(t, 5, nil)
	req := request(t, 3)
	reg, j, _, err := h.s.TrySubmitAndWatch(h.ctx, req)
	if err != nil || reg == nil || !reserved(j) || j.QueuePosition != nil {
		t.Fatalf("no-wait on idle broker: %v %+v", err, j)
	}
	if again, dup, _, err := h.s.TrySubmitAndWatch(h.ctx, req); err != nil || dup.JobId != j.JobId || again == nil || h.s.Current(reg) {
		t.Fatalf("same-incarnation recovery: %v", err)
	}
	stranger := proto.Clone(req).(*pb.SubmitJobRequest)
	stranger.Runner = &pb.RunnerIncarnation{IncarnationId: uuid4(t), Process: &pb.ProcessIdentity{Pid: 9, StartTimeUnixNs: 9}}
	if _, _, _, err := h.s.TrySubmitAndWatch(h.ctx, stranger); !errors.Is(err, storage.ErrKeyReused) {
		t.Fatalf("other incarnation recovery: %v", err)
	}
	if _, _, err := h.s.Submit(h.ctx, req); !errors.Is(err, storage.ErrKeyReused) {
		t.Fatalf("mode change: %v", err)
	}

	_, head, _ := h.queued(3)
	_, unregistered := h.submit(1)
	n := request(t, 1)
	if _, nj, _, err := h.s.TrySubmitAndWatch(h.ctx, n); err != nil || !nj.AdmittedByBackfill {
		t.Fatalf("no-wait backfill past unregistered follower: %v", err)
	}
	if j := h.job(head); j.BackfillBypassCount != 1 {
		t.Fatalf("bypass count %d", j.BackfillBypassCount)
	}
	h.expectState(unregistered, pb.JobState_JOB_STATE_QUEUED)
	busy := request(t, 1)
	if reg, _, _, err := h.s.TrySubmitAndWatch(h.ctx, busy); !errors.Is(err, storage.ErrAdmissionBusy) || reg != nil {
		t.Fatalf("bypass limit: %v", err)
	}
	h.cancel(unregistered)
	h.cancel(j.JobId)
	h.expectState(head, pb.JobState_JOB_STATE_RESERVED)
	for _, retry := range []*pb.SubmitJobRequest{busy, func() *pb.SubmitJobRequest { r := request(t, 1); r.IdempotencyKey = busy.IdempotencyKey; return r }()} {
		if _, _, _, err := h.s.TrySubmitAndWatch(h.ctx, retry); !errors.Is(err, storage.ErrAdmissionBusy) {
			t.Fatalf("busy key not stable: %v", err)
		}
		if _, _, err := h.s.Submit(h.ctx, retry); !errors.Is(err, storage.ErrAdmissionBusy) {
			t.Fatalf("busy key through Submit: %v", err)
		}
	}
	if _, _, _, err := h.s.TrySubmitAndWatch(h.ctx, request(t, 6)); !errors.Is(err, ErrWeightExceedsCapacity) {
		t.Fatalf("oversized no-wait: %v", err)
	}
	if code := status.Convert(ContractError(storage.ErrAdmissionBusy)); code.Code().String() != "FailedPrecondition" {
		t.Fatalf("busy maps to %v", code.Code())
	}
}

func TestNoWaitRespectsOlderWorkPauseAndClosedStreams(t *testing.T) {
	h := newHarness(t, 2, nil)
	if _, err := h.s.SetQueueMode(h.ctx, pb.QueueMode_QUEUE_MODE_PAUSED); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := h.s.TrySubmitAndWatch(h.ctx, request(t, 1)); !errors.Is(err, storage.ErrAdmissionBusy) {
		t.Fatalf("paused no-wait: %v", err)
	}
	if _, err := h.s.SetQueueMode(h.ctx, pb.QueueMode_QUEUE_MODE_ENABLED); err != nil {
		t.Fatal(err)
	}
	h.queued(2)
	if _, _, _, err := h.s.TrySubmitAndWatch(h.ctx, request(t, 1)); !errors.Is(err, storage.ErrAdmissionBusy) {
		t.Fatalf("full no-wait: %v", err)
	}
	cancelled, cancel := context.WithCancel(h.ctx)
	cancel()
	closed := request(t, 1)
	if _, _, _, err := h.s.TrySubmitAndWatch(cancelled, closed); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("closed stream: %v", err)
	}
	err := h.store.View(h.ctx, func(r *storage.Reader) error { _, err := r.Submission(closed.IdempotencyKey); return err })
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("closed stream left a record: %v", err)
	}
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := h.s.TrySubmitAndWatch(h.ctx, request(t, 1)); err == nil || errors.Is(err, storage.ErrAdmissionBusy) {
		t.Fatalf("storage failure reported as %v", err)
	}
}

func TestStreamClosingDuringDecisionKeepsStorageUsable(t *testing.T) {
	h := newHarness(t, 2, nil)
	h.queued(1)
	req, id := h.submit(1)
	stream, closeStream := context.WithCancel(h.ctx)
	h.sampler.mu.Lock()
	h.sampler.during = closeStream
	h.sampler.mu.Unlock()
	if _, _, _, err := h.s.Watch(stream, id, req.Runner); err != nil {
		t.Fatalf("watch: %v", err)
	}
	if err := h.store.Failure(); err != nil {
		t.Fatalf("storage poisoned: %v", err)
	}
	// The closing stream must not turn into a resource refusal for others.
	h.expectState(id, pb.JobState_JOB_STATE_RESERVED)

	h.cancel(id)
	stream, closeStream = context.WithCancel(h.ctx)
	h.sampler.mu.Lock()
	h.sampler.during = closeStream
	h.sampler.mu.Unlock()
	closed := request(t, 1)
	if _, _, _, err := h.s.TrySubmitAndWatch(stream, closed); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("stream closed during no-wait decision: %v", err)
	}
	err := h.store.View(h.ctx, func(r *storage.Reader) error { _, err := r.Submission(closed.IdempotencyKey); return err })
	if !errors.Is(err, storage.ErrNotFound) || h.store.Failure() != nil {
		t.Fatalf("closed no-wait stream left a record or failure: %v %v", err, h.store.Failure())
	}
}

func TestNotReadyRejectsNewWorkButAcceptsReconnect(t *testing.T) {
	h := newHarness(t, 2, nil)
	req, id := h.submit(1)
	if err := h.s.Stop(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.store.Close()
	var err error
	if h.store, err = storage.Open(h.ctx, h.path); err != nil {
		t.Fatal(err)
	}
	h.s, _, err = Start(h.ctx, h.store, Options{Config: h.cfg, ServiceVersion: "test", PID: 1, Clock: h.clock.read, Sampler: h.sampler,
		Processes: h.probe, Publish: h.publish})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.s.Submit(h.ctx, request(t, 1)); !errors.Is(err, ErrNotReady) {
		t.Fatalf("submit while recovering: %v", err)
	}
	if _, _, _, err := h.s.TrySubmitAndWatch(h.ctx, request(t, 1)); !errors.Is(err, ErrNotReady) {
		t.Fatalf("no-wait while recovering: %v", err)
	}
	h.watch(req, id)
	snap := h.snapshot()
	if !snap.Status.Recovering || snap.Status.HeadBlockedReason != pb.BlockedReason_BLOCKED_REASON_RECOVERING {
		t.Fatalf("status %+v", snap.Status)
	}
	h.expectState(id, pb.JobState_JOB_STATE_QUEUED)
	if _, err := h.s.MarkReady(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.expectState(id, pb.JobState_JOB_STATE_RESERVED)
}

func TestEventsCarryPolicyProjection(t *testing.T) {
	h := newHarness(t, 2, nil)
	_, events, err := h.s.Submit(h.ctx, request(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	var sawJob, sawQueue bool
	for _, e := range events {
		if j := e.GetJobChanged(); j != nil {
			sawJob = j.GraceRemainingMs != nil && *j.GraceRemainingMs == 30000
		}
		if q := e.GetQueueChanged(); q != nil {
			sawQueue = q.HeadBlockedReason == pb.BlockedReason_BLOCKED_REASON_RUNNER_DISCONNECTED &&
				q.QueueState == pb.QueueState_QUEUE_STATE_BLOCKED
		}
	}
	if !sawJob || !sawQueue {
		t.Fatalf("events not projected: %v", events)
	}
}

func TestQueueModeChangesEmitStatusAndQueueAndAreIdempotent(t *testing.T) {
	h := newHarness(t, 2, nil)
	check := func(mode pb.QueueMode, state pb.QueueState, wantEvents bool) {
		t.Helper()
		events, err := h.s.SetQueueMode(h.ctx, mode)
		if err != nil {
			t.Fatal(err)
		}
		var status *pb.BrokerStatus
		var queue *pb.Queue
		for _, e := range events {
			if s := e.GetStatusChanged(); s != nil {
				status = s
			}
			if q := e.GetQueueChanged(); q != nil {
				queue = q
			}
		}
		if !wantEvents {
			if len(events) != 0 {
				t.Fatalf("idempotent %v emitted %v", mode, events)
			}
			return
		}
		if status == nil || queue == nil || status.QueueMode != mode || status.QueueState != state || queue.QueueState != state ||
			status.Paused != (mode == pb.QueueMode_QUEUE_MODE_PAUSED) {
			t.Fatalf("mode %v events %v", mode, events)
		}
	}
	if s := h.snapshot().Status; s.QueueMode != pb.QueueMode_QUEUE_MODE_ENABLED || s.QueueState != pb.QueueState_QUEUE_STATE_IDLE {
		t.Fatalf("initial %+v", s)
	}
	check(pb.QueueMode_QUEUE_MODE_PAUSED, pb.QueueState_QUEUE_STATE_PAUSED, true)
	check(pb.QueueMode_QUEUE_MODE_PAUSED, 0, false)
	check(pb.QueueMode_QUEUE_MODE_ENABLED, pb.QueueState_QUEUE_STATE_IDLE, true)
	check(pb.QueueMode_QUEUE_MODE_ENABLED, 0, false)
	if _, err := h.s.SetQueueMode(h.ctx, pb.QueueMode_QUEUE_MODE_UNSPECIFIED); err == nil {
		t.Fatal("unspecified mode accepted")
	}
}
