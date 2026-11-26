package scheduler

import (
	"context"

	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"github.com/boumenot/heavy-lock/apps/tuna/storage"
)

// SaveResources checkpoints an active job's historical measurements without
// consulting them in any scheduling decision.
func (s *Scheduler) SaveResources(ctx context.Context, id string, summary *pb.ResourceSummary, finished bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(false); err != nil {
		return err
	}
	_, err := s.write(ctx, func(tx *storage.Tx) error {
		tx.ProjectEvent = s.project(s.ready)
		job, err := tx.Job(id)
		if err != nil {
			return err
		}
		isFinished := job.State == pb.JobState_JOB_STATE_FINISHED
		if isFinished != finished {
			return storage.ErrTransition
		}
		if isFinished && job.Resources != nil && job.Resources.ElapsedMs != nil &&
			(summary.ElapsedMs == nil || *summary.ElapsedMs != *job.Resources.ElapsedMs) {
			return storage.ErrTransition
		}
		return tx.SaveResources(id, summary)
	})
	return err
}
