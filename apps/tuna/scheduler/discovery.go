package scheduler

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/boumenot/tuna/apps/tuna/contract"
	pb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
	"github.com/boumenot/tuna/apps/tuna/jobmetrics"
	"github.com/boumenot/tuna/apps/tuna/storage"
)

type discoveryState struct {
	root     jobmetrics.Identity
	inFlight bool
	ready    bool
	// generation counts committed complete passes for this root.
	generation uint64
}

type discoveryTarget struct {
	jobID   string
	root    jobmetrics.Identity
	anchors []storage.Process
}

type discoveredProcess struct {
	identity jobmetrics.Identity
	role     string
}

type discoveryResult struct {
	target    discoveryTarget
	processes []discoveredProcess
	err       error
}

func processIdentity(p *pb.ProcessIdentity) jobmetrics.Identity {
	return jobmetrics.Identity{PID: p.GetPid(), StartTimeUnixNs: startNanos(p)}
}

// startNanos returns 0, an invalid identity, when the start time is absent or invalid.
func startNanos(p *pb.ProcessIdentity) int64 {
	start, err := contract.UnixNano(p.GetStartTime())
	if err != nil {
		return 0
	}
	return start
}

// Discover records positive, stable ancestry observations without treating
// omissions as exit evidence. The scan barrier remains closed until those
// observations commit; sampling itself never holds the scheduling mutex.
func (s *Scheduler) Discover(ctx context.Context, source jobmetrics.Source) error {
	s.discoveryMu.Lock()
	defer s.discoveryMu.Unlock()

	s.mu.Lock()
	if err := s.usable(false); err != nil {
		s.mu.Unlock()
		return err
	}
	var targets []discoveryTarget
	err := s.store.View(ctx, func(r *storage.Reader) error {
		jobs, err := r.ActiveJobs()
		if err != nil {
			return err
		}
		for _, j := range jobs {
			if j.Process == nil {
				continue
			}
			anchors, err := r.Processes(j.JobId)
			if err != nil {
				return err
			}
			targets = append(targets, discoveryTarget{jobID: j.JobId,
				root: processIdentity(j.Process), anchors: anchors})
		}
		return nil
	})
	if err != nil {
		for id, state := range s.discovery {
			state.ready = false
			s.discovery[id] = state
		}
		s.mu.Unlock()
		return fmt.Errorf("read discovery targets: %w", err)
	}
	for _, target := range targets {
		state := s.discovery[target.jobID]
		if state.root != target.root {
			state = discoveryState{root: target.root}
		}
		state.inFlight = true
		s.discovery[target.jobID] = state
	}
	s.mu.Unlock()
	if len(targets) == 0 {
		return nil
	}

	var snapshot jobmetrics.Snapshot
	if source == nil {
		err = errors.New("process discovery source is unavailable")
	} else {
		snapshot, err = source.Snapshot(ctx)
	}
	if err == nil && snapshot.IdentityFailure != "" {
		err = fmt.Errorf("process identity verification: %s", snapshot.IdentityFailure)
	}
	var results []discoveryResult
	if err == nil {
		results = discoverTargets(targets, snapshot)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		_, err = s.write(ctx, func(tx *storage.Tx) error {
			for _, result := range results {
				job, err := tx.Job(result.target.jobID)
				if err != nil {
					return err
				}
				if job.State == pb.JobState_JOB_STATE_FINISHED ||
					processIdentity(job.Process) != result.target.root {
					return fmt.Errorf("job %s changed during discovery", result.target.jobID)
				}
				for _, p := range result.processes {
					identity := &pb.ProcessIdentity{Pid: p.identity.PID,
						StartTime: contract.Timestamp(p.identity.StartTimeUnixNs)}
					if err := tx.TrackProcess(result.target.jobID, identity, p.role); err != nil {
						return fmt.Errorf("track job %s PID %d: %w", result.target.jobID, p.identity.PID, err)
					}
				}
			}
			return nil
		})
	}
	var gaps []error
	for i, target := range targets {
		state := s.discovery[target.jobID]
		state.inFlight = false
		state.ready = err == nil && results[i].err == nil
		if state.ready {
			state.generation++
		}
		s.discovery[target.jobID] = state
		if err == nil && results[i].err != nil {
			gaps = append(gaps, fmt.Errorf("job %s: %w", target.jobID, results[i].err))
		}
	}
	if err != nil {
		return fmt.Errorf("process discovery: %w", err)
	}
	return errors.Join(gaps...)
}

func discoverTargets(targets []discoveryTarget, snapshot jobmetrics.Snapshot) []discoveryResult {
	byPID := make(map[int64][]jobmetrics.Process, len(snapshot.Processes))
	children := make(map[int64][]jobmetrics.Process)
	for _, p := range snapshot.Processes {
		byPID[p.Identity.PID] = append(byPID[p.Identity.PID], p)
		children[p.ParentPID] = append(children[p.ParentPID], p)
	}
	results := make([]discoveryResult, 0, len(targets))
	for _, target := range targets {
		result := discoveryResult{target: target}
		known := make(map[jobmetrics.Identity]string)
		for _, anchor := range target.anchors {
			if anchor.Role == "command" || anchor.Role == "descendant" {
				known[processIdentity(anchor.Identity)] = anchor.Role
			}
		}
		var queue []jobmetrics.Process
		var gaps []error
		for id := range known {
			rows := byPID[id.PID]
			if len(rows) == 0 {
				continue
			}
			if len(rows) != 1 {
				gaps = append(gaps, fmt.Errorf("duplicate observed PID %d", id.PID))
				continue
			}
			p := rows[0]
			if p.Identity.StartTimeUnixNs <= 0 {
				gaps = append(gaps, fmt.Errorf("invalid identity for known PID %d", id.PID))
				continue
			}
			if p.Unstable {
				gaps = append(gaps, fmt.Errorf("unstable known PID %d", id.PID))
				continue
			}
			if p.Identity != id {
				continue
			}
			queue = append(queue, p)
		}
		seen := make(map[jobmetrics.Identity]bool)
		for len(queue) > 0 {
			p := queue[0]
			queue = queue[1:]
			if seen[p.Identity] {
				continue
			}
			seen[p.Identity] = true
			role := known[p.Identity]
			if role == "" {
				role = "descendant"
			}
			result.processes = append(result.processes, discoveredProcess{identity: p.Identity, role: role})
			for _, child := range children[p.Identity.PID] {
				if child.Identity.PID <= 0 || child.Identity.PID > math.MaxInt32 ||
					child.Identity.StartTimeUnixNs <= 0 {
					gaps = append(gaps, fmt.Errorf("invalid child identity of PID %d", p.Identity.PID))
					continue
				}
				if len(byPID[child.Identity.PID]) != 1 {
					gaps = append(gaps, fmt.Errorf("duplicate observed PID %d", child.Identity.PID))
					continue
				}
				if child.Unstable {
					gaps = append(gaps, fmt.Errorf("unstable descendant PID %d", child.Identity.PID))
					continue
				}
				queue = append(queue, child)
			}
		}
		result.err = errors.Join(gaps...)
		results = append(results, result)
	}
	return results
}
