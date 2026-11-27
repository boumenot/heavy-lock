package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"github.com/boumenot/heavy-lock/apps/tuna/storage"
	"google.golang.org/protobuf/proto"
)

// LaunchTimeout is the investigation threshold from reservation commit to an
// accepted claim and start report. Expiry is never release evidence.
const LaunchTimeout = 30 * time.Second

const (
	CancelAckTimeout        = 5 * time.Second
	CancelCompletionTimeout = 30 * time.Second
)

const launchInvestigationDetail = "launch investigation expired without a reported root or no-launch proof"

const takeoverBlockedDetail = "broker takeover is blocked: verified identity-bound signaling and coordination are unavailable"

// probeTimeout bounds one OS process check inside a release transaction.
const probeTimeout = 2 * time.Second

// ErrGrantFenced rejects a claim whose grant can no longer launch: it was
// cancelled, fenced or released.
var ErrGrantFenced = errors.New("grant is fenced and cannot be claimed")

// ProcessProbe reports OS evidence that a tracked process has exited. It
// must return true only when the identity (PID and start time) is verified
// gone or replaced; any doubt is false or an error, which keeps the
// reservation held.
type ProcessProbe interface {
	Exited(ctx context.Context, p *pb.ProcessIdentity) (bool, error)
}

type probeIdentity struct {
	role       string
	pid, start int64
}

type probeWarning struct {
	jobID    string
	identity probeIdentity
	err      error
}

func (s *Scheduler) probeExited(ctx context.Context, jobID, role string, p *pb.ProcessIdentity, warnings *[]probeWarning) (bool, error) {
	key := probeIdentity{role: role, pid: p.GetPid(), start: startNanos(p)}
	var exited bool
	var err error
	if s.processes == nil {
		err = errors.New("process probe is unavailable")
	} else {
		probeCtx, cancel := context.WithTimeout(durable(ctx), probeTimeout)
		exited, err = s.processes.Exited(probeCtx, p)
		cancel()
	}
	failures := s.probeFailures[jobID]
	if err == nil {
		delete(failures, key)
		if len(failures) == 0 {
			delete(s.probeFailures, jobID)
		}
		return exited, nil
	}
	if failures == nil {
		failures = map[probeIdentity]string{}
		s.probeFailures[jobID] = failures
	}
	if previous, ok := failures[key]; !ok || previous != err.Error() {
		*warnings = append(*warnings, probeWarning{jobID: jobID, identity: key, err: err})
	}
	failures[key] = err.Error()
	return false, err
}

func (s *Scheduler) unlockAndWarn(warnings *[]probeWarning) {
	s.mu.Unlock()
	for _, warning := range *warnings {
		s.warnf("job %s: %s process state unverifiable (PID %d, start %d): %v",
			warning.jobID, warning.identity.role, warning.identity.pid, warning.identity.start, warning.err)
	}
}

type cancelClock struct {
	acknowledged bool
	since        time.Duration
}

func (s *Scheduler) updateCancelClock(job *pb.Job, now time.Duration) {
	if job.State == pb.JobState_JOB_STATE_FINISHED || job.Grant == nil ||
		job.CancelRequestedAt == nil || job.BrokerTermination != nil {
		delete(s.cancellation, job.JobId)
		return
	}
	acknowledged := job.CancelAcknowledgedAt != nil
	if clock, ok := s.cancellation[job.JobId]; !ok || clock.acknowledged != acknowledged {
		s.cancellation[job.JobId] = cancelClock{acknowledged: acknowledged, since: now}
	}
}

func (s *Scheduler) takeoverReason(job *pb.Job, runnerDead bool, now time.Duration) pb.BrokerTermination_Reason {
	if runnerDead {
		return pb.BrokerTermination_REASON_RUNNER_DEAD
	}
	if job.CancelRequestedAt == nil {
		return pb.BrokerTermination_REASON_UNSPECIFIED
	}
	if job.CancelFailureDetail != nil {
		return pb.BrokerTermination_REASON_CANCEL_RUNNER_FAILED
	}
	clock, ok := s.cancellation[job.JobId]
	if !ok || clock.acknowledged != (job.CancelAcknowledgedAt != nil) {
		return pb.BrokerTermination_REASON_UNSPECIFIED
	}
	if clock.acknowledged && now-clock.since >= s.cfg.CancelCompletionTimeout {
		return pb.BrokerTermination_REASON_CANCEL_COMPLETION_DEADLINE
	}
	if !clock.acknowledged && now-clock.since >= s.cfg.CancelAckTimeout {
		return pb.BrokerTermination_REASON_CANCEL_ACK_DEADLINE
	}
	return pb.BrokerTermination_REASON_UNSPECIFIED
}

func needsLaunchInvestigation(job *pb.Job) bool {
	if job.State != pb.JobState_JOB_STATE_RESERVED || job.Grant == nil || job.Process != nil {
		return false
	}
	if job.Outcome == nil {
		return true
	}
	return job.Outcome.Kind == pb.OutcomeKind_OUTCOME_KIND_UNKNOWN &&
		!strings.Contains(job.Outcome.GetDetail(), launchInvestigationDetail)
}

func markUnknown(tx *storage.Tx, job *pb.Job, detail string) error {
	if job.Outcome != nil {
		if job.Outcome.Kind != pb.OutcomeKind_OUTCOME_KIND_UNKNOWN || strings.Contains(job.Outcome.GetDetail(), detail) {
			return nil
		}
		if previous := job.Outcome.GetDetail(); previous != "" {
			detail = previous + "; " + detail
		}
	}
	return tx.MarkUnknown(job.JobId, &detail)
}

// Cancel records a cancellation request. A queued job finishes at once; a
// reserved, unclaimed job is fenced and released; a claimed job keeps its
// reservation until the runner's reports and process evidence allow release.
func (s *Scheduler) Cancel(ctx context.Context, id string, reason *string) ([]*pb.BrokerEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(false); err != nil {
		return nil, err
	}
	events, err := s.write(ctx, func(tx *storage.Tx) error {
		tx.ProjectEvent = s.project(s.ready)
		return tx.Cancel(id, reason)
	})
	if err != nil {
		return nil, err
	}
	delete(s.grace, id)
	more, err := s.admitLocked(ctx)
	return append(events, more...), err
}

// Claim persists the one-shot launch claim for the grant's incarnation. A
// retry by the same incarnation returns alreadyClaimed rather than a second
// allowance. Claims are refused until the broker is ready.
func (s *Scheduler) Claim(ctx context.Context, id, grant string, runner *pb.RunnerIncarnation) (*pb.Job, bool, []*pb.BrokerEvent, error) {
	var already bool
	job, events, err := s.runnerOp(ctx, id, true, func(tx *storage.Tx) error {
		var err error
		already, err = tx.Claim(id, grant, runner)
		if errors.Is(err, storage.ErrTransition) {
			return fmt.Errorf("%w: %w", ErrGrantFenced, err)
		}
		return err
	})
	return job, already, events, err
}

// Seal records that the runner fenced all future spawns under the grant.
func (s *Scheduler) Seal(ctx context.Context, id, grant string, runner *pb.RunnerIncarnation, noSpawn bool) (*pb.Job, []*pb.BrokerEvent, error) {
	return s.runnerOp(ctx, id, false, func(tx *storage.Tx) error { return tx.Seal(id, grant, runner, noSpawn) })
}

func (s *Scheduler) ReportStarted(ctx context.Context, id, grant string, runner *pb.RunnerIncarnation, p *pb.ProcessIdentity) (*pb.Job, []*pb.BrokerEvent, error) {
	return s.runnerOp(ctx, id, false, func(tx *storage.Tx) error { return tx.ReportStarted(id, grant, runner, p) })
}

func (s *Scheduler) ReportFinished(ctx context.Context, id, grant string, runner *pb.RunnerIncarnation, p *pb.ProcessIdentity, outcome *pb.Outcome) (*pb.Job, []*pb.BrokerEvent, error) {
	return s.ReportFinishedWithResources(ctx, id, grant, runner, p, outcome, nil)
}

// ReportFinishedWithResources commits the runner's outcome and final
// resource summary in the same transaction, before any possible release.
func (s *Scheduler) ReportFinishedWithResources(ctx context.Context, id, grant string, runner *pb.RunnerIncarnation, p *pb.ProcessIdentity, outcome *pb.Outcome, summary *pb.ResourceSummary) (*pb.Job, []*pb.BrokerEvent, error) {
	return s.runnerOp(ctx, id, false, func(tx *storage.Tx) error {
		before, err := tx.Job(id)
		if err != nil {
			return err
		}
		if err := tx.ReportFinished(id, grant, runner, p, outcome); err != nil {
			return err
		}
		if summary == nil {
			return nil
		}
		if before.Outcome != nil && before.Outcome.Kind != pb.OutcomeKind_OUTCOME_KIND_UNKNOWN && before.Resources != nil {
			if before.Resources.Elapsed != nil && summary.Elapsed != nil && !proto.Equal(before.Resources.Elapsed, summary.Elapsed) {
				return storage.ErrIdentity
			}
			return nil
		}
		return tx.SaveResources(id, summary)
	})
}

func (s *Scheduler) ReportLaunchFailed(ctx context.Context, id, grant string, runner *pb.RunnerIncarnation, detail string, cancelled bool) (*pb.Job, []*pb.BrokerEvent, error) {
	return s.runnerOp(ctx, id, false, func(tx *storage.Tx) error { return tx.ReportLaunchFailed(id, grant, runner, detail, cancelled) })
}

func (s *Scheduler) AcknowledgeCancel(ctx context.Context, id, grant string, runner *pb.RunnerIncarnation) (*pb.Job, []*pb.BrokerEvent, error) {
	return s.runnerOp(ctx, id, false, func(tx *storage.Tx) error { return tx.AcknowledgeCancel(id, grant, runner) })
}

func (s *Scheduler) ReportCancelFailed(ctx context.Context, id, grant string, runner *pb.RunnerIncarnation, detail string) (*pb.Job, []*pb.BrokerEvent, error) {
	return s.runnerOp(ctx, id, false, func(tx *storage.Tx) error { return tx.ReportCancelFailed(id, grant, runner, detail) })
}

// Rejoin returns a job to its own runner without any launch permission.
func (s *Scheduler) Rejoin(ctx context.Context, id string, runner *pb.RunnerIncarnation) (*pb.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(false); err != nil {
		return nil, err
	}
	var job *pb.Job
	err := s.store.View(ctx, func(r *storage.Reader) error {
		var err error
		if job, err = r.Job(id); err != nil {
			return err
		}
		switch job.State {
		case pb.JobState_JOB_STATE_FINISHED:
		case pb.JobState_JOB_STATE_QUEUED:
			g, err := r.QueueGrace(id)
			if err != nil {
				return err
			}
			if !sameRunner(g.Runner, runner) {
				return storage.ErrIdentity
			}
		default:
			if job.Grant == nil || !sameRunner(job.Grant.Runner, runner) {
				return storage.ErrIdentity
			}
		}
		return s.projectJob(r, job)
	})
	return job, err
}

// runnerOp applies one runner lifecycle transition and, in the same
// transaction, any release it makes provable. A release runs admission.
func (s *Scheduler) runnerOp(ctx context.Context, id string, needReady bool, op func(*storage.Tx) error) (*pb.Job, []*pb.BrokerEvent, error) {
	var warnings []probeWarning
	s.mu.Lock()
	defer s.unlockAndWarn(&warnings)
	if err := s.usable(needReady); err != nil {
		return nil, nil, err
	}
	var job *pb.Job
	released := false
	events, err := s.write(ctx, func(tx *storage.Tx) error {
		tx.ProjectEvent = s.project(s.ready)
		if err := op(tx); err != nil {
			return err
		}
		var err error
		if released, err = s.reconcile(ctx, tx, id, &warnings); err != nil {
			return err
		}
		if job, err = tx.Job(id); err != nil {
			return err
		}
		return s.projectJob(&tx.Reader, job)
	})
	if err != nil {
		return nil, nil, err
	}
	if !released {
		return job, events, nil
	}
	more, err := s.admitLocked(ctx)
	return job, append(events, more...), err
}

// Reconcile retries release for every active reservation, for example on a
// periodic tick after a process probe was inconclusive, and then runs
// admission if anything was released.
func (s *Scheduler) Reconcile(ctx context.Context) ([]*pb.BrokerEvent, error) {
	var warnings []probeWarning
	s.mu.Lock()
	defer s.unlockAndWarn(&warnings)
	if err := s.usable(false); err != nil {
		return nil, err
	}
	released := false
	var investigated []string
	now := s.clock()
	events, err := s.write(ctx, func(tx *storage.Tx) error {
		tx.ProjectEvent = s.project(s.ready)
		released = false
		investigated = investigated[:0]
		jobs, err := tx.ActiveJobs()
		if err != nil {
			return err
		}
		for _, j := range jobs {
			dead := false
			if s.ready {
				res, err := tx.Reservation(j.JobId)
				if err != nil {
					return err
				}
				dead, err = s.probeExited(ctx, j.JobId, "runner", res.Runner.Process, &warnings)
				if err != nil && s.processes != nil {
					detail := fmt.Sprintf("runner process state unverifiable: %v", err)
					if err := markUnknown(tx, j, detail); err != nil {
						return err
					}
				} else if dead {
					if err := tx.TrackProcess(j.JobId, res.Runner.Process, "runner"); err != nil {
						return err
					}
					if err := tx.VerifyProcessExited(j.JobId, res.Runner.Process); err != nil {
						return err
					}
					if err := tx.FenceRunnerTerminated(j.JobId, res.Runner); err != nil {
						return err
					}
					if err := markUnknown(tx, j, "assigned runner death verified; native outcome is unknown"); err != nil {
						return err
					}
				}
				j, err = tx.Job(j.JobId)
				if err != nil {
					return err
				}
			}
			if since, ok := s.launch[j.JobId]; s.ready && ok && now-since >= s.cfg.LaunchTimeout {
				res, err := tx.Reservation(j.JobId)
				if err != nil {
					return err
				}
				if j.Process == nil && res.NoLaunchProof == nil {
					if err := markUnknown(tx, j, launchInvestigationDetail+"; reservation remains held"); err != nil {
						return err
					}
				}
				investigated = append(investigated, j.JobId)
			}
			ok, err := s.reconcile(ctx, tx, j.JobId, &warnings)
			if err != nil {
				return err
			}
			if s.ready && !ok {
				current, err := tx.Job(j.JobId)
				if err != nil {
					return err
				}
				if current.BrokerTermination == nil {
					if reason := s.takeoverReason(current, dead, now); reason != pb.BrokerTermination_REASON_UNSPECIFIED {
						if err := tx.RecordTermination(j.JobId, reason); err != nil {
							return err
						}
						if err := markUnknown(tx, current, takeoverBlockedDetail); err != nil {
							return err
						}
					}
				}
			}
			released = released || ok
		}
		return nil
	})
	if err != nil {
		return events, err
	}
	for _, id := range investigated {
		delete(s.launch, id)
	}
	if !released {
		return events, nil
	}
	more, err := s.admitLocked(ctx)
	return append(events, more...), err
}

// reconcile releases a fenced reservation once its evidence is complete:
// every tracked non-runner process verified exited by the OS probe, and an
// outcome that matches the release reason. Anything short of that, including
// a missing probe or incomplete/in-flight discovery, keeps the reservation
// held. Discovery is best effort; never-observed descendants may be missed.
func (s *Scheduler) reconcile(ctx context.Context, tx *storage.Tx, id string, warnings *[]probeWarning) (bool, error) {
	res, err := tx.Reservation(id)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil || res.ReleasedAt != nil || res.FencedAt == nil {
		return false, err
	}
	job, err := tx.Job(id)
	if err != nil {
		return false, err
	}
	if job.Process != nil {
		state, ok := s.discovery[id]
		if !ok || state.root != processIdentity(job.Process) || !state.ready || state.inFlight {
			return false, nil
		}
	}
	processes, err := tx.Processes(id)
	if err != nil {
		return false, err
	}
	verified := true
	for _, p := range processes {
		if p.Role == "runner" || p.ExitedVerifiedAt != nil {
			continue
		}
		exited, err := s.probeExited(ctx, id, p.Role, p.Identity, warnings)
		if err != nil || !exited {
			verified = false
			continue
		}
		if err := tx.VerifyProcessExited(id, p.Identity); err != nil {
			return false, err
		}
	}
	if !verified {
		return false, nil
	}
	var reason string
	switch kind := job.GetOutcome().GetKind(); {
	case res.NoLaunchProof != nil && kind == pb.OutcomeKind_OUTCOME_KIND_LAUNCH_FAILED:
		reason = "launch_failed"
	case res.NoLaunchProof != nil && kind == pb.OutcomeKind_OUTCOME_KIND_CANCELLED_BEFORE_START:
		reason = "cancelled_before_start"
	case job.Process != nil && (kind == pb.OutcomeKind_OUTCOME_KIND_EXITED || kind == pb.OutcomeKind_OUTCOME_KIND_SIGNALED):
		reason = "processes_exited"
	case kind == pb.OutcomeKind_OUTCOME_KIND_UNKNOWN && (job.Process != nil || res.NoLaunchProof != nil):
		reason = "recovery_verified_gone"
	default:
		return false, nil
	}
	err = tx.Release(id, reason)
	if errors.Is(err, storage.ErrUnreconciled) {
		return false, nil
	}
	return err == nil, err
}
