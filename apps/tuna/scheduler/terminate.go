package scheduler

import (
	"context"
	"errors"
	"fmt"
	"syscall"

	pb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
	"github.com/boumenot/tuna/apps/tuna/procid"
	"github.com/boumenot/tuna/apps/tuna/storage"
)

// MaxFreezeRounds bounds how many takeover passes wait for discovery to
// settle, stopping newly found processes, before the broker kills the
// tracked processes anyway.
const MaxFreezeRounds = 10

// ProcessTerminator delivers a fingerprint-checked signal to a tracked
// process. A nil error means the signal was sent, not that the process
// exited; only the ProcessProbe can verify exit. Errors wrapping
// ErrSignalMismatch mean the PID no longer carries the recorded identity and
// nothing was signalled.
type ProcessTerminator interface {
	Signal(ctx context.Context, p *pb.ProcessIdentity, sig syscall.Signal) error
}

// ErrSignalMismatch is wrapped by terminators that skipped a signal because
// the PID did not match its recorded identity.
var ErrSignalMismatch = procid.ErrMismatch

type terminationState struct {
	stopped  map[probeIdentity]bool
	freezeAt uint64 // discovery generation when the last process was stopped
	rounds   int    // takeover passes before killing
	killing  bool
	logged   map[string]bool
}

type terminationTarget struct {
	jobID     string
	processes []storage.Process
}

type signalResult struct {
	jobID    string
	identity probeIdentity
	sig      syscall.Signal
	err      error
}

// Terminate force-stops the tracked processes of jobs the broker has taken
// over. It first stops every known process so a parent cannot start new
// children, waits for a discovery pass that finds no new process (or for
// MaxFreezeRounds passes), and then kills them all. It writes nothing: the
// reservation is released only by Reconcile after every tracked process is
// verified exited. The runner process is never signalled.
func (s *Scheduler) Terminate(ctx context.Context) error {
	s.mu.Lock()
	if err := s.usable(false); err != nil || !s.ready || s.terminator == nil {
		s.mu.Unlock()
		return err
	}
	var targets []terminationTarget
	err := s.store.View(ctx, func(r *storage.Reader) error {
		jobs, err := r.ActiveJobs()
		if err != nil {
			return err
		}
		for _, j := range jobs {
			if j.BrokerTermination == nil {
				continue
			}
			res, err := r.Reservation(j.JobId)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if res.ReleasedAt != nil {
				continue
			}
			processes, err := r.Processes(j.JobId)
			if err != nil {
				return err
			}
			target := terminationTarget{jobID: j.JobId}
			for _, p := range processes {
				if p.Role != "runner" && p.ExitedVerifiedAt == nil {
					target.processes = append(target.processes, p)
				}
			}
			targets = append(targets, target)
		}
		return nil
	})
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("read takeover targets: %w", err)
	}
	active := make(map[string]bool, len(targets))
	type plan struct {
		jobID     string
		sig       syscall.Signal
		processes []storage.Process
	}
	var plans []plan
	for _, target := range targets {
		active[target.jobID] = true
		state, ok := s.termination[target.jobID]
		if !ok {
			state = &terminationState{stopped: map[probeIdentity]bool{}, logged: map[string]bool{}}
			s.termination[target.jobID] = state
		}
		var fresh []storage.Process
		for _, p := range target.processes {
			if !state.stopped[probeKey(p)] {
				fresh = append(fresh, p)
			}
		}
		discovery := s.discovery[target.jobID]
		settled := discovery.ready && !discovery.inFlight && discovery.generation > state.freezeAt
		if !state.killing {
			state.rounds++
			switch {
			case len(fresh) > 0 && state.rounds < MaxFreezeRounds:
				state.freezeAt = discovery.generation
				plans = append(plans, plan{jobID: target.jobID, sig: syscall.SIGSTOP, processes: fresh})
				continue
			case len(fresh) == 0 && !settled && state.rounds < MaxFreezeRounds:
				continue
			}
			state.killing = true
		}
		if len(target.processes) > 0 {
			plans = append(plans, plan{jobID: target.jobID, sig: syscall.SIGKILL, processes: target.processes})
		}
	}
	for id := range s.termination {
		if !active[id] {
			delete(s.termination, id)
		}
	}
	terminator := s.terminator
	s.mu.Unlock()

	var results []signalResult
	for _, plan := range plans {
		for _, p := range plan.processes {
			err := terminator.Signal(ctx, p.Identity, plan.sig)
			results = append(results, signalResult{jobID: plan.jobID, identity: probeKey(p), sig: plan.sig, err: err})
		}
	}

	s.mu.Lock()
	var messages []string
	for _, r := range results {
		state, ok := s.termination[r.jobID]
		if !ok {
			continue
		}
		if r.sig == syscall.SIGSTOP && (r.err == nil || errors.Is(r.err, ErrSignalMismatch)) {
			state.stopped[r.identity] = true
		}
		outcome := "sent"
		if errors.Is(r.err, ErrSignalMismatch) {
			outcome = "skipped"
		} else if r.err != nil {
			outcome = "failed"
		}
		key := fmt.Sprintf("%d/%d/%d/%s", r.identity.pid, r.identity.start, r.sig, outcome)
		if state.logged[key] {
			continue
		}
		state.logged[key] = true
		message := fmt.Sprintf("job %s: takeover %s %s to %s process (PID %d, start %d)",
			r.jobID, outcome, signalName(r.sig), r.identity.role, r.identity.pid, r.identity.start)
		if r.err != nil {
			message += fmt.Sprintf(": %v", r.err)
		}
		messages = append(messages, message+"; reservation remains held until exit is verified")
	}
	s.mu.Unlock()
	for _, message := range messages {
		s.warnf("%s", message)
	}
	return nil
}

func probeKey(p storage.Process) probeIdentity {
	return probeIdentity{role: p.Role, pid: p.Identity.GetPid(), start: startNanos(p.Identity)}
}

func signalName(sig syscall.Signal) string {
	switch sig {
	case syscall.SIGSTOP:
		return "SIGSTOP"
	case syscall.SIGKILL:
		return "SIGKILL"
	}
	return sig.String()
}
