package scheduler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/boumenot/heavy-lock/apps/tuna/contract"
	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"github.com/boumenot/heavy-lock/apps/tuna/storage"
)

var (
	ErrNotReady              = errors.New("broker is not ready for new admissions")
	ErrStopped               = errors.New("scheduler stopped")
	ErrWeightExceedsCapacity = errors.New("weight exceeds the configured capacity")
	ErrNotWaiting            = errors.New("job is not a registered waiting job")
	// ErrStreamClosed aborts a no-wait call whose stream was known closed
	// before the decision; it is not an admission refusal.
	ErrStreamClosed = errors.New("stream closed before the admission decision")
)

const sampleTimeout = 5 * time.Second

// candidateID names the no-wait candidate during its decision; it cannot
// collide with a UUID job ID.
const candidateID = "\x00no-wait-candidate"

// Clock returns monotonic time since an arbitrary origin within one broker
// run. Broker downtime is never measured by it.
type Clock func() time.Duration

func MonotonicClock() Clock {
	start := time.Now()
	return func() time.Duration { return time.Since(start) }
}

type Options struct {
	Config         Config
	ServiceVersion string
	PID            int64
	Clock          Clock
	Sampler        Sampler
	// Publish, when set, receives every committed event batch in commit
	// order, under the scheduler mutex. It must not block or call back into
	// the scheduler.
	Publish func([]*pb.BrokerEvent)
	// Processes verifies that tracked processes have exited before a
	// release. Without it no process is ever verified, so capacity held by
	// a launched command stays held (fail closed).
	Processes ProcessProbe
}

// Registration is one accepted job stream. Only the latest registration of
// a job is current; a superseded registration's disconnect is ignored.
type Registration struct {
	JobID string
	// Seq is the last committed event sequence reflected by the snapshot
	// returned with this registration. A stream subscribed to live events
	// before registering skips events at or below it.
	Seq   int64
	token uint64
}

type disconnected struct {
	base  time.Duration // elapsed grace carried from durable checkpoints
	since time.Duration // clock reading when counting resumed
}

// Scheduler is the single serialized scheduling authority for one broker
// run. Every operation holds its mutex across the storage transaction, so
// admission, controls, stream registration/disconnection and grace expiry
// are totally ordered. Returned events are committed and publishable; they
// are returned even when a later step of the same call fails.
type Scheduler struct {
	mu            sync.Mutex
	store         *storage.Store
	cfg           Config
	version       string
	run           int64
	clock         Clock
	sampler       Sampler
	ready         bool
	stopped       bool
	lastAdmission time.Duration
	streams       map[string]*Registration
	tokens        uint64
	grace         map[string]disconnected
	lastSample    *Sample
	lastSharing   *Sharing
	publish       func([]*pb.BrokerEvent)
	processes     ProcessProbe
	lastSeq       int64
}

// Start records a new broker run with capacity fixed for that run. Every
// queued runner's stream from the previous run is gone: connected entries
// become disconnected with a fresh grace period, and unregistered entries
// resume from their durable checkpoint without charging downtime. Grace
// counts from here because existing jobs may reconnect during recovery.
// Normal admission waits for MarkReady.
func Start(ctx context.Context, store *storage.Store, o Options) (*Scheduler, []*pb.BrokerEvent, error) {
	if err := o.Config.validate(); err != nil {
		return nil, nil, err
	}
	if store == nil || o.Sampler == nil {
		return nil, nil, errors.New("storage and resource sampler are required")
	}
	if o.Clock == nil {
		o.Clock = MonotonicClock()
	}
	s := &Scheduler{store: store, cfg: o.Config, version: o.ServiceVersion, clock: o.Clock,
		sampler: o.Sampler, streams: map[string]*Registration{}, grace: map[string]disconnected{},
		publish: o.Publish, processes: o.Processes}
	now := s.clock()
	// The previous admission time is unknown after a restart; settle from now.
	s.lastAdmission = now
	events, err := s.write(ctx, func(tx *storage.Tx) error {
		tx.ProjectEvent = s.project(false)
		clear(s.grace)
		var err error
		if s.run, err = tx.StartService(o.ServiceVersion, o.PID); err != nil {
			return err
		}
		queue, err := tx.Queue()
		if err != nil {
			return err
		}
		for _, j := range queue.Jobs {
			g, err := tx.QueueGrace(j.JobId)
			if err != nil {
				return err
			}
			if g.Connected {
				s.grace[j.JobId] = disconnected{since: now}
				err = tx.SetQueueConnection(j.JobId, g.Runner, false, s.run)
			} else {
				s.grace[j.JobId] = disconnected{base: time.Duration(g.ElapsedUS) * time.Microsecond, since: now}
				err = tx.CheckpointGrace(j.JobId, g.ElapsedUS, s.run)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return s, events, nil
}

// MarkReady is called once recovery trusts complete durable accounting. It
// commits the ready marker and current status/queue snapshots, then runs
// normal admission.
func (s *Scheduler) MarkReady(ctx context.Context) ([]*pb.BrokerEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, ErrStopped
	}
	events, err := s.write(ctx, func(tx *storage.Tx) error {
		tx.ProjectEvent = s.project(true)
		return tx.ReadyService(s.run)
	})
	if err != nil {
		return nil, err
	}
	s.ready = true
	more, err := s.admitLocked(ctx)
	return append(events, more...), err
}

// Stop checkpoints grace so downtime is not charged, then records a clean
// shutdown. Reservations and queue entries are preserved.
func (s *Scheduler) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil
	}
	now := s.clock()
	_, err := s.write(ctx, func(tx *storage.Tx) error {
		tx.ProjectEvent = s.project(false)
		for _, id := range s.graceIDs() {
			j, err := tx.Job(id)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if j.State == pb.JobState_JOB_STATE_QUEUED && !j.RunnerConnected {
				if err := tx.CheckpointGrace(id, s.elapsed(id, now).Microseconds(), s.run); err != nil {
					return err
				}
			}
		}
		return tx.StopService(s.run)
	})
	if err == nil {
		s.stopped = true
		s.ready = false
	}
	return err
}

func (s *Scheduler) ServiceRunID() int64 { return s.run }

// Ready reports whether normal admission is open (MarkReady committed and
// the scheduler is not stopped).
func (s *Scheduler) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready && !s.stopped
}

// write is the only path to a storage write: it detaches the commit from
// caller cancellation and publishes the committed events exactly once, in
// commit order. Callers hold the mutex (Start runs before any other use).
func (s *Scheduler) write(ctx context.Context, mutate func(*storage.Tx) error) ([]*pb.BrokerEvent, error) {
	events, err := s.store.Write(durable(ctx), mutate)
	if err == nil && len(events) > 0 {
		s.lastSeq = events[len(events)-1].EventSeq
	}
	if err == nil && s.publish != nil {
		s.publish(events)
	}
	return events, err
}

// EventSeq returns the last committed event sequence of this broker run.
// Every write goes through write, so it is the store's current sequence.
func (s *Scheduler) EventSeq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSeq
}

func (s *Scheduler) usable(needReady bool) error {
	if s.stopped {
		return ErrStopped
	}
	if needReady && !s.ready {
		return ErrNotReady
	}
	return nil
}

// Submit queues an ordinary submission unregistered, with grace running
// from its commit. A duplicate returns the existing job without resetting
// grace; a key with a committed no-wait refusal returns ErrAdmissionBusy.
func (s *Scheduler) Submit(ctx context.Context, req *pb.SubmitJobRequest) (storage.SubmissionResult, []*pb.BrokerEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result storage.SubmissionResult
	if err := s.usable(true); err != nil {
		return result, nil, err
	}
	now := s.clock()
	events, err := s.write(ctx, func(tx *storage.Tx) error {
		tx.ProjectEvent = s.project(true)
		if _, err := s.fresh(tx, req); err != nil {
			return err
		}
		var err error
		result, err = tx.Submit(req, storage.Submission{Mode: contract.AdmissionQueued,
			ServiceVersion: s.version, ServiceRunID: s.run})
		if err != nil || result.Busy {
			return err
		}
		return s.projectJob(&tx.Reader, result.Job)
	})
	if err != nil {
		return storage.SubmissionResult{}, nil, err
	}
	if result.Busy {
		return result, events, storage.ErrAdmissionBusy
	}
	if result.Created {
		s.grace[result.Job.JobId] = disconnected{since: now}
	}
	return result, events, nil
}

// fresh reports whether the key is new, checking existing records before
// any fresh-request validation such as the capacity limit.
func (s *Scheduler) fresh(tx *storage.Tx, req *pb.SubmitJobRequest) (bool, error) {
	_, err := tx.Submission(req.GetIdempotencyKey())
	switch {
	case errors.Is(err, storage.ErrNotFound):
		if Oversized(req.GetWeight(), s.cfg.Capacity) {
			return true, fmt.Errorf("%w: weight %d, capacity %d", ErrWeightExceedsCapacity, req.GetWeight(), s.cfg.Capacity)
		}
		return true, nil
	case err == nil, errors.Is(err, storage.ErrKeyDeleted), errors.Is(err, storage.ErrAdmissionBusy):
		// tx.Submit returns the durable result for existing keys.
		return false, nil
	default:
		return false, err
	}
}

// TrySubmitAndWatch registers the calling stream and makes exactly one
// normal-admission decision, as if the candidate were appended to the
// queue. It reserves the candidate only when it is the next normal choice;
// otherwise it commits a key-only refusal and returns ErrAdmissionBusy.
// Any other error, including a failed commit, is not a busy result.
// ctx must be the stream's context: a stream known closed before the
// decision aborts without a reservation or refusal.
func (s *Scheduler) TrySubmitAndWatch(ctx context.Context, req *pb.SubmitJobRequest) (*Registration, *pb.Job, []*pb.BrokerEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(true); err != nil {
		return nil, nil, nil, err
	}
	if ctx.Err() != nil {
		return nil, nil, nil, ErrStreamClosed
	}
	var result storage.SubmissionResult
	p := &probe{s: s, ctx: durable(ctx)}
	events, err := s.write(ctx, func(tx *storage.Tx) error {
		tx.ProjectEvent = s.project(true)
		fresh, err := s.fresh(tx, req)
		if err != nil {
			return err
		}
		options := storage.Submission{Mode: contract.AdmissionNoWait, ServiceVersion: s.version, ServiceRunID: s.run}
		if fresh {
			state, err := s.state(&tx.Reader, true, p.sharing)
			if err != nil {
				return err
			}
			state.Queue = append(state.Queue, Entry{JobID: candidateID, Weight: req.GetWeight(), Connected: true})
			d := Decide(state)
			if ctx.Err() != nil {
				// Nothing has been written; the rollback leaves no record.
				return ErrStreamClosed
			}
			s.remember(d)
			if d.Admit == candidateID {
				options.Admission = &storage.Admission{BackfillHead: d.BackfillHead, FreeMemoryBytes: p.freeMemory()}
			}
		}
		if result, err = tx.Submit(req, options); err != nil || result.Busy {
			return err
		}
		if result.Created {
			// Conservative if the commit then fails: it only delays sharing.
			s.lastAdmission = s.clock()
		} else if result.Job.State != pb.JobState_JOB_STATE_FINISHED &&
			(result.Job.Grant == nil || !sameRunner(result.Job.Grant.Runner, req.GetRunner())) {
			// Same-key recovery never grants another incarnation an allowance.
			return storage.ErrIdentity
		}
		return s.projectJob(&tx.Reader, result.Job)
	})
	if err != nil {
		return nil, nil, nil, err
	}
	if result.Busy {
		return nil, nil, events, storage.ErrAdmissionBusy
	}
	if result.Job.State == pb.JobState_JOB_STATE_FINISHED {
		return nil, result.Job, events, nil
	}
	reg := s.register(result.Job.JobId)
	reg.Seq = s.lastSeq
	return reg, result.Job, events, nil
}

// durable detaches storage writes and samples from caller cancellation. A
// context cancelled at COMMIT would otherwise make storage permanently
// unavailable, and a closing stream must not refuse sharing for other jobs.
// Callers that must observe cancellation check it explicitly.
func durable(ctx context.Context) context.Context { return context.WithoutCancel(ctx) }

func (s *Scheduler) register(id string) *Registration {
	s.tokens++
	r := &Registration{JobID: id, token: s.tokens}
	s.streams[id] = r
	return r
}

// Current reports whether r is the job's latest accepted registration.
func (s *Scheduler) Current(r *Registration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return r != nil && s.streams[r.JobID] == r
}

func sameRunner(a, b *pb.RunnerIncarnation) bool {
	return a != nil && b != nil && a.Process != nil && b.Process != nil &&
		a.IncarnationId == b.IncarnationId && a.Process.Pid == b.Process.Pid &&
		a.Process.StartTimeUnixNs == b.Process.StartTimeUnixNs
}

// Watch registers a job stream. For a queued job, the submitting
// incarnation's stream registers (or reconnects) the entry and resets grace;
// it supersedes any older stream. A terminal job returns its snapshot with
// no registration; an expired job is never revived. Existing jobs may watch
// before ready. A non-nil registration remains valid even if the following
// admission pass reports an error.
func (s *Scheduler) Watch(ctx context.Context, id string, runner *pb.RunnerIncarnation) (*Registration, *pb.Job, []*pb.BrokerEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(false); err != nil {
		return nil, nil, nil, err
	}
	previous := s.streams[id]
	reg := s.register(id) // visible to event projection inside the transaction
	restore := func() {
		if previous == nil {
			delete(s.streams, id)
		} else {
			s.streams[id] = previous
		}
	}
	var job *pb.Job
	queued := false
	events, err := s.write(ctx, func(tx *storage.Tx) error {
		tx.ProjectEvent = s.project(s.ready)
		j, err := tx.Job(id)
		if err != nil {
			return err
		}
		switch j.State {
		case pb.JobState_JOB_STATE_QUEUED:
			queued = true
			if err := tx.SetQueueConnection(id, runner, true, s.run); err != nil {
				return err
			}
		case pb.JobState_JOB_STATE_FINISHED:
		default:
			if j.Grant == nil || !sameRunner(j.Grant.Runner, runner) {
				return storage.ErrIdentity
			}
		}
		job, err = tx.Job(id)
		if err != nil {
			return err
		}
		return s.projectJob(&tx.Reader, job)
	})
	if err != nil {
		restore()
		return nil, nil, nil, err
	}
	if job.State == pb.JobState_JOB_STATE_FINISHED {
		restore()
		return nil, job, events, nil
	}
	if queued {
		delete(s.grace, id)
	}
	reg.Seq = s.lastSeq
	more, err := s.admitLocked(ctx)
	return reg, job, append(events, more...), err
}

// Disconnect handles the end of a registered stream. A superseded stream is
// ignored. A queued job keeps its place and starts a fresh grace period;
// other jobs keep their reservations: disconnection never releases capacity.
// The stream's own context is usually already cancelled, so the durable
// update ignores cancellation. If it fails, Tick reconciles the job.
func (s *Scheduler) Disconnect(ctx context.Context, r *Registration) ([]*pb.BrokerEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r == nil || s.streams[r.JobID] != r {
		return nil, nil
	}
	delete(s.streams, r.JobID)
	if s.stopped {
		return nil, nil
	}
	now := s.clock()
	disconnectedQueued := false
	events, err := s.write(ctx, func(tx *storage.Tx) error {
		tx.ProjectEvent = s.project(s.ready)
		var err error
		disconnectedQueued, err = s.disconnectQueued(tx, r.JobID, now)
		return err
	})
	if err != nil {
		return nil, err
	}
	if disconnectedQueued {
		s.grace[r.JobID] = disconnected{since: now}
	}
	return events, nil
}

// disconnectQueued durably marks a connected queued job disconnected with
// fresh grace. The caller records in-memory grace only after commit; the
// projection already reports the fresh period.
func (s *Scheduler) disconnectQueued(tx *storage.Tx, id string, now time.Duration) (bool, error) {
	j, err := tx.Job(id)
	if err != nil || j.State != pb.JobState_JOB_STATE_QUEUED || !j.RunnerConnected {
		return false, err
	}
	g, err := tx.QueueGrace(id)
	if err != nil {
		return false, err
	}
	previous, tracked := s.grace[id]
	s.grace[id] = disconnected{since: now}
	err = tx.SetQueueConnection(id, g.Runner, false, s.run)
	if tracked {
		s.grace[id] = previous
	} else {
		delete(s.grace, id)
	}
	return err == nil, err
}

func (s *Scheduler) graceIDs() []string {
	ids := make([]string, 0, len(s.grace))
	for id := range s.grace {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (s *Scheduler) elapsed(id string, now time.Duration) time.Duration {
	g := s.grace[id]
	return g.base + max(now-g.since, 0)
}

// NextGraceDeadline returns the earliest clock reading at which a tracked
// disconnected queued job's grace expires.
func (s *Scheduler) NextGraceDeadline() (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var next time.Duration
	found := false
	for _, g := range s.grace {
		deadline := g.since + s.cfg.Grace - g.base
		if !found || deadline < next {
			next, found = deadline, true
		}
	}
	return next, found
}

// Tick removes queued jobs whose grace expired, checkpoints the rest, and
// runs normal admission (settling and resource checks change over time).
// Expiry rechecks durable connection state in the same transaction, so a
// reconnect committed first prevents removal.
func (s *Scheduler) Tick(ctx context.Context) ([]*pb.BrokerEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(false); err != nil {
		return nil, err
	}
	now := s.clock()
	var drop, started []string
	events, err := s.write(ctx, func(tx *storage.Tx) error {
		tx.ProjectEvent = s.project(s.ready)
		drop, started = drop[:0], started[:0]
		for _, id := range s.graceIDs() {
			j, err := tx.Job(id)
			if errors.Is(err, storage.ErrNotFound) {
				drop = append(drop, id)
				continue
			}
			if err != nil {
				return err
			}
			if j.State != pb.JobState_JOB_STATE_QUEUED || j.RunnerConnected {
				drop = append(drop, id)
				continue
			}
			elapsed := s.elapsed(id, now)
			if elapsed >= s.cfg.Grace {
				if err := tx.RemoveDisconnected(id); err != nil {
					return err
				}
				drop = append(drop, id)
			} else if err := tx.CheckpointGrace(id, elapsed.Microseconds(), s.run); err != nil {
				return err
			}
		}
		// A durably connected queued job without a live stream (for example
		// after a failed Disconnect commit) is disconnected now.
		queue, err := tx.Queue()
		if err != nil {
			return err
		}
		for _, j := range queue.Jobs {
			if !j.RunnerConnected || s.streams[j.JobId] != nil {
				continue
			}
			if _, err := s.disconnectQueued(tx, j.JobId, now); err != nil {
				return err
			}
			started = append(started, j.JobId)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, id := range drop {
		delete(s.grace, id)
	}
	for _, id := range started {
		s.grace[id] = disconnected{since: now}
	}
	more, err := s.admitLocked(ctx)
	return append(events, more...), err
}

// Schedule runs normal admission, for example after a reservation is released.
func (s *Scheduler) Schedule(ctx context.Context) ([]*pb.BrokerEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(false); err != nil {
		return nil, err
	}
	return s.admitLocked(ctx)
}

// ForceStart reserves a registered queued job out of order, bypassing pause,
// capacity (including oversized weights) and sharing checks. It never
// bypasses readiness, counts its full weight as occupied and is not a
// backfill bypass.
func (s *Scheduler) ForceStart(ctx context.Context, id string) (*pb.Job, []*pb.BrokerEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(true); err != nil {
		return nil, nil, err
	}
	var job *pb.Job
	p := &probe{s: s, ctx: durable(ctx)}
	events, err := s.write(ctx, func(tx *storage.Tx) error {
		tx.ProjectEvent = s.project(true)
		j, err := tx.Job(id)
		if err != nil {
			return err
		}
		if j.State != pb.JobState_JOB_STATE_QUEUED {
			return fmt.Errorf("%w: job is %s", ErrNotWaiting, j.State)
		}
		if !j.RunnerConnected || s.streams[id] == nil {
			return fmt.Errorf("%w: runner is not registered", ErrNotWaiting)
		}
		s.lastAdmission = s.clock()
		if err := tx.Reserve(id, s.run, storage.Admission{Forced: true, FreeMemoryBytes: p.freeMemory()}); err != nil {
			return err
		}
		job, err = tx.Job(id)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return job, events, nil
}

// Pause persistently stops or resumes normal admission. Running and already
// reserved work is unaffected; force-start remains available.
func (s *Scheduler) Pause(ctx context.Context, paused bool) ([]*pb.BrokerEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(false); err != nil {
		return nil, err
	}
	events, err := s.write(ctx, func(tx *storage.Tx) error {
		tx.ProjectEvent = s.project(s.ready)
		return tx.Pause(paused)
	})
	if err != nil || paused {
		return events, err
	}
	more, err := s.admitLocked(ctx)
	return append(events, more...), err
}

// MoveBefore reorders a waiting job before another; an empty before moves
// it to the back. Reorder never transfers or resets bypass counts.
func (s *Scheduler) MoveBefore(ctx context.Context, id, before string) ([]*pb.BrokerEvent, error) {
	return s.move(ctx, id, func(*storage.Tx) (string, error) { return before, nil })
}

func (s *Scheduler) MoveToFront(ctx context.Context, id string) ([]*pb.BrokerEvent, error) {
	return s.move(ctx, id, func(tx *storage.Tx) (string, error) {
		queue, err := tx.Queue()
		if err != nil || len(queue.Jobs) == 0 {
			return "", errors.Join(err, ErrNotWaiting)
		}
		return queue.Jobs[0].JobId, nil
	})
}

func (s *Scheduler) move(ctx context.Context, id string, target func(*storage.Tx) (string, error)) ([]*pb.BrokerEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(false); err != nil {
		return nil, err
	}
	events, err := s.write(ctx, func(tx *storage.Tx) error {
		tx.ProjectEvent = s.project(s.ready)
		if _, err := tx.Job(id); err != nil {
			return err
		}
		before, err := target(tx)
		if err != nil {
			return err
		}
		if err := tx.MoveBefore(id, before); errors.Is(err, storage.ErrTransition) {
			return fmt.Errorf("%w: %w", ErrNotWaiting, err)
		} else if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	more, err := s.admitLocked(ctx)
	return append(events, more...), err
}

// admitLocked commits normal admissions one transaction at a time until
// the policy admits nothing. Each decision is made inside the transaction
// that commits it.
func (s *Scheduler) admitLocked(ctx context.Context) ([]*pb.BrokerEvent, error) {
	var all []*pb.BrokerEvent
	for s.ready && !s.stopped {
		admitted := false
		p := &probe{s: s, ctx: durable(ctx)}
		events, err := s.write(ctx, func(tx *storage.Tx) error {
			tx.ProjectEvent = s.project(true)
			admitted = false
			state, err := s.state(&tx.Reader, true, p.sharing)
			if err != nil {
				return err
			}
			d := Decide(state)
			s.remember(d)
			if d.Admit == "" {
				return nil
			}
			admitted = true
			s.lastAdmission = s.clock()
			return tx.Reserve(d.Admit, s.run, storage.Admission{BackfillHead: d.BackfillHead, FreeMemoryBytes: p.freeMemory()})
		})
		all = append(all, events...)
		if err != nil || !admitted {
			return all, err
		}
	}
	return all, nil
}

func (s *Scheduler) remember(d Decision) {
	if d.Sharing != nil {
		sharing := *d.Sharing
		s.lastSharing = &sharing
	}
}

func (s *Scheduler) state(r *storage.Reader, ready bool, sharing func() Sharing) (State, error) {
	status, err := r.Status()
	if err != nil {
		return State{}, err
	}
	queue, err := r.Queue()
	if err != nil {
		return State{}, err
	}
	return s.stateOf(status, queue, ready, sharing), nil
}

func (s *Scheduler) stateOf(status *pb.BrokerStatus, queue *pb.Queue, ready bool, sharing func() Sharing) State {
	state := State{Capacity: s.cfg.Capacity, BackfillLimit: s.cfg.BackfillLimit, Ready: ready,
		Paused: status.Paused, Occupied: status.Occupied, Sharing: sharing}
	for _, j := range queue.Jobs {
		state.Queue = append(state.Queue, Entry{JobID: j.JobId, Weight: j.Weight,
			Connected: j.RunnerConnected && s.streams[j.JobId] != nil, BypassCount: j.BackfillBypassCount})
	}
	return state
}

// probe samples live resources at most once per decision, and only when a
// sharing check or admission record needs it.
type probe struct {
	s       *Scheduler
	ctx     context.Context
	sampled bool
	sample  *Sample
}

func (p *probe) read() *Sample {
	if !p.sampled {
		p.sampled = true
		ctx, cancel := context.WithTimeout(p.ctx, sampleTimeout)
		defer cancel()
		if sample, err := p.s.sampler.Sample(ctx); err == nil && sample.TotalMemoryBytes > 0 {
			p.sample = &sample
			p.s.lastSample = &sample
		}
	}
	return p.sample
}

func (p *probe) sharing() Sharing {
	since := p.s.clock() - p.s.lastAdmission
	var sample *Sample
	if since >= p.s.cfg.Settle {
		sample = p.read()
	}
	reason := p.s.cfg.SharingRefusal(sample, since)
	return Sharing{Allowed: reason == "", Reason: reason}
}

func (p *probe) freeMemory() *int64 {
	sample := p.read()
	if sample == nil {
		return nil
	}
	free := int64(min(sample.AvailableMemoryBytes, 1<<62))
	return &free
}
