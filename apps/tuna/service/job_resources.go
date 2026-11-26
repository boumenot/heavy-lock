package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"github.com/boumenot/heavy-lock/apps/tuna/jobmetrics"
	"github.com/boumenot/heavy-lock/apps/tuna/scheduler"
	"github.com/boumenot/heavy-lock/apps/tuna/storage"
	"google.golang.org/protobuf/proto"
)

const (
	jobSampleInterval     = 5 * time.Second
	jobCheckpointInterval = 60 * time.Second
	jobSampleTimeout      = 3 * time.Second
)

type resourceScheduler interface {
	Snapshot(context.Context) (*pb.BrokerSnapshot, error)
	Job(context.Context, string) (*pb.Job, error)
	SaveResources(context.Context, string, *pb.ResourceSummary, bool) error
}

type resourceEntry struct {
	root       jobmetrics.Identity
	aggregate  *jobmetrics.Aggregate
	checkpoint time.Duration
}

type resourceManager struct {
	mu         sync.Mutex
	scheduler  resourceScheduler
	source     jobmetrics.Source
	clock      scheduler.Clock
	entries    map[string]*resourceEntry
	lastSample time.Duration
	hasSample  bool
}

func newResourceManager(s resourceScheduler, source jobmetrics.Source) *resourceManager {
	return &resourceManager{
		scheduler: s, source: source, clock: scheduler.MonotonicClock(),
		entries: make(map[string]*resourceEntry),
	}
}

func resourceIdentity(p *pb.ProcessIdentity) jobmetrics.Identity {
	return jobmetrics.Identity{PID: p.GetPid(), StartTimeUnixNs: p.GetStartTimeUnixNs()}
}

// restore runs before admission opens, making any lost checkpoint interval
// visible immediately instead of presenting the previous summary as complete.
func (m *resourceManager) restore(ctx context.Context) error {
	snapshot, err := m.scheduler.Snapshot(ctx)
	if err != nil {
		return err
	}
	now := m.clock()
	for _, job := range snapshot.ActiveJobs {
		if job.Process == nil {
			continue
		}
		root := resourceIdentity(job.Process)
		entry := &resourceEntry{root: root, aggregate: jobmetrics.New(root, job.Resources, true), checkpoint: now}
		if err := m.scheduler.SaveResources(ctx, job.JobId, entry.aggregate.Summary(), false); err != nil {
			return err
		}
		m.entries[job.JobId] = entry
	}
	return nil
}

func (m *resourceManager) run(ctx context.Context, logf func(string, ...any)) {
	ticker := time.NewTicker(jobSampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.sample(ctx); err != nil && ctx.Err() == nil {
				logf("job resource sampling: %v", err)
			}
		}
	}
}

func (m *resourceManager) sample(ctx context.Context) error {
	active, err := m.scheduler.Snapshot(ctx)
	if err != nil {
		return err
	}
	hasRoot := false
	for _, job := range active.ActiveJobs {
		hasRoot = hasRoot || job.Process != nil
	}
	var observed jobmetrics.Snapshot
	var observationErr error
	if hasRoot {
		sampleCtx, cancel := context.WithTimeout(ctx, jobSampleTimeout)
		observed, observationErr = m.source.Snapshot(sampleCtx)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	now := m.clock()
	m.mu.Lock()
	defer m.mu.Unlock()
	delayed := hasRoot && m.hasSample && now-m.lastSample > 2*jobSampleInterval
	m.lastSample, m.hasSample = now, hasRoot
	var failures []error
	if observationErr != nil {
		failures = append(failures, fmt.Errorf("process observation: %w", observationErr))
	}
	for _, job := range active.ActiveJobs {
		if job.Process == nil {
			continue
		}
		current, err := m.scheduler.Job(ctx, job.JobId)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if current.State == pb.JobState_JOB_STATE_FINISHED || current.Process == nil ||
			!proto.Equal(current.Process, job.Process) {
			continue
		}
		root := resourceIdentity(current.Process)
		entry := m.entries[job.JobId]
		if entry == nil {
			entry = &resourceEntry{root: root, aggregate: jobmetrics.New(root, current.Resources, current.Resources != nil), checkpoint: now}
			m.entries[job.JobId] = entry
		}
		if entry.root != root {
			entry.aggregate.Failure("recorded root identity changed")
			continue
		}
		if delayed {
			entry.aggregate.Failure("resource sampling was delayed by more than one interval")
		}
		if observationErr != nil {
			entry.aggregate.Failure("process enumeration failed: " + observationErr.Error())
		} else {
			entry.aggregate.Observe(now, observed)
		}
		if now-entry.checkpoint >= jobCheckpointInterval {
			err := m.scheduler.SaveResources(ctx, job.JobId, entry.aggregate.Summary(), false)
			if err == nil {
				entry.checkpoint = now
			} else if !errors.Is(err, storage.ErrTransition) && !errors.Is(err, storage.ErrNotFound) {
				failures = append(failures, fmt.Errorf("checkpoint %s: %w", job.JobId, err))
			}
		}
	}
	for id, entry := range m.entries {
		job, err := m.scheduler.Job(ctx, id)
		if errors.Is(err, storage.ErrNotFound) {
			delete(m.entries, id)
			continue
		}
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if job.State != pb.JobState_JOB_STATE_FINISHED {
			continue
		}
		final := entry.aggregate.Clone()
		if job.Resources == nil || job.Resources.ElapsedMs == nil {
			final.Finish(nil, nil)
		}
		if err := m.scheduler.SaveResources(ctx, id, final.Summary(), true); err != nil && !errors.Is(err, storage.ErrNotFound) {
			failures = append(failures, fmt.Errorf("finish summary %s: %w", id, err))
		} else {
			delete(m.entries, id)
		}
	}
	return errors.Join(failures...)
}

// Finish holds the observational state stable until the scheduler has
// accepted the report and committed its final summary with the outcome.
func (m *resourceManager) Finish(ctx context.Context, req *pb.ReportFinishedRequest, commit func(*pb.ResourceSummary) (*pb.Job, error)) (*pb.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, err := m.scheduler.Job(ctx, req.JobId)
	if err != nil {
		return nil, err
	}
	if job.Outcome != nil && job.Outcome.Kind != pb.OutcomeKind_OUTCOME_KIND_UNKNOWN && job.Resources != nil {
		summary := proto.Clone(job.Resources).(*pb.ResourceSummary)
		if req.ElapsedMs != nil {
			summary.ElapsedMs = proto.Int64(*req.ElapsedMs)
		}
		return commit(summary)
	}
	root := resourceIdentity(req.Process)
	entry := m.entries[req.JobId]
	if entry != nil && entry.root != root {
		return nil, storage.ErrIdentity
	}
	if entry == nil {
		entry = &resourceEntry{root: root, aggregate: jobmetrics.New(root, job.Resources, job.Resources != nil), checkpoint: m.clock()}
	}
	final := entry.aggregate.Clone()
	final.Finish(req.ElapsedMs, req.RootCpuTimeMs)
	result, err := commit(final.Summary())
	if result != nil {
		if result.State == pb.JobState_JOB_STATE_FINISHED {
			delete(m.entries, req.JobId)
		} else {
			entry.aggregate = final
			m.entries[req.JobId] = entry
		}
	}
	return result, err
}
