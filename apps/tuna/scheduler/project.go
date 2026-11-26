package scheduler

import (
	"context"
	"math"

	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"github.com/boumenot/heavy-lock/apps/tuna/storage"
)

// project fills policy-derived fields of committed events. It never samples
// resources: the head reason reflects the last live sample and settle clock.
func (s *Scheduler) project(ready bool) func(*storage.Reader, *pb.BrokerEvent) error {
	return func(r *storage.Reader, event *pb.BrokerEvent) error {
		switch {
		case event.GetJobChanged() != nil:
			return s.projectJob(r, event.GetJobChanged())
		case event.GetQueueChanged() != nil:
			return s.projectQueue(r, event.GetQueueChanged(), ready)
		case event.GetStatusChanged() != nil:
			return s.projectStatus(r, event.GetStatusChanged(), ready)
		}
		return nil
	}
}

func (s *Scheduler) projectJob(r *storage.Reader, j *pb.Job) error {
	j.GraceRemainingMs = nil
	j.BlockedReason = pb.BlockedReason_BLOCKED_REASON_UNSPECIFIED
	if j.State != pb.JobState_JOB_STATE_QUEUED {
		return nil
	}
	if Oversized(j.Weight, s.cfg.Capacity) {
		j.BlockedReason = pb.BlockedReason_BLOCKED_REASON_WEIGHT_EXCEEDS_CAPACITY
	}
	if j.RunnerConnected {
		return nil
	}
	var elapsed int64
	if _, ok := s.grace[j.JobId]; ok {
		elapsed = s.elapsed(j.JobId, s.clock()).Milliseconds()
	} else {
		g, err := r.QueueGrace(j.JobId)
		if err != nil {
			return err
		}
		elapsed = g.ElapsedUS / 1000
	}
	remaining := max(s.cfg.Grace.Milliseconds()-elapsed, 0)
	j.GraceRemainingMs = &remaining
	return nil
}

func (s *Scheduler) headBlocked(status *pb.BrokerStatus, queue *pb.Queue, ready bool) pb.BlockedReason {
	view := func() Sharing {
		since := s.clock() - s.lastAdmission
		reason := s.cfg.SharingRefusal(s.lastSample, since)
		return Sharing{Allowed: reason == "", Reason: reason}
	}
	return Decide(s.stateOf(status, queue, ready, view)).HeadBlocked
}

func (s *Scheduler) projectQueue(r *storage.Reader, q *pb.Queue, ready bool) error {
	for _, j := range q.Jobs {
		if err := s.projectJob(r, j); err != nil {
			return err
		}
	}
	status, err := r.Status()
	if err != nil {
		return err
	}
	q.HeadBlockedReason = s.headBlocked(status, q, ready)
	return nil
}

func (s *Scheduler) projectStatus(r *storage.Reader, st *pb.BrokerStatus, ready bool) error {
	queue, err := r.Queue()
	if err != nil {
		return err
	}
	st.Capacity = s.cfg.Capacity
	st.BackfillLimit = s.cfg.BackfillLimit
	st.Recovering = !ready
	st.HeadBlockedReason = s.headBlocked(st, queue, ready)
	st.FreeMemoryBytes, st.LoadAverage_1M = nil, nil
	if sample := s.lastSample; sample != nil {
		free := int64(min(sample.AvailableMemoryBytes, math.MaxInt64))
		load := sample.LoadAverage1m
		st.FreeMemoryBytes, st.LoadAverage_1M = &free, &load
	}
	return nil
}

// Snapshot reads a consistent, policy-projected view of status, queue and
// active jobs.
func (s *Scheduler) Snapshot(ctx context.Context) (*pb.BrokerSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := &pb.BrokerSnapshot{}
	err := s.store.View(ctx, func(r *storage.Reader) error {
		var err error
		if snapshot.Status, err = r.Status(); err != nil {
			return err
		}
		if snapshot.Queue, err = r.Queue(); err != nil {
			return err
		}
		if snapshot.ActiveJobs, err = r.ActiveJobs(); err != nil {
			return err
		}
		if err := s.projectStatus(r, snapshot.Status, s.ready); err != nil {
			return err
		}
		return s.projectQueue(r, snapshot.Queue, s.ready)
	})
	return snapshot, err
}

// Job reads one policy-projected job.
func (s *Scheduler) Job(ctx context.Context, id string) (*pb.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var job *pb.Job
	err := s.store.View(ctx, func(r *storage.Reader) error {
		var err error
		if job, err = r.Job(id); err != nil {
			return err
		}
		return s.projectJob(r, job)
	})
	return job, err
}
