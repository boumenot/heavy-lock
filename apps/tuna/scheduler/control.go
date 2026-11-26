package scheduler

import (
	"context"
	"time"

	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"github.com/boumenot/heavy-lock/apps/tuna/storage"
)

func (s *Scheduler) ListJobs(ctx context.Context, req *pb.ListJobsRequest) (*pb.ListJobsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result *pb.ListJobsResponse
	err := s.store.View(ctx, func(r *storage.Reader) error {
		var err error
		result, err = r.ListJobs(req)
		if err != nil {
			return err
		}
		for _, job := range result.Jobs {
			if err := s.projectJob(r, job); err != nil {
				return err
			}
		}
		return nil
	})
	return result, err
}

func (s *Scheduler) Submission(ctx context.Context, key string) (*pb.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var job *pb.Job
	err := s.store.View(ctx, func(r *storage.Reader) error {
		var err error
		job, err = r.Submission(key)
		if err != nil {
			return err
		}
		return s.projectJob(r, job)
	})
	return job, err
}

func (s *Scheduler) MoveAfter(ctx context.Context, id, after string) ([]*pb.BrokerEvent, error) {
	return s.move(ctx, id, func(tx *storage.Tx) (string, error) {
		queue, err := tx.Queue()
		if err != nil {
			return "", err
		}
		for i, job := range queue.Jobs {
			if job.JobId == after {
				if i+1 == len(queue.Jobs) {
					return "", nil
				}
				return queue.Jobs[i+1].JobId, nil
			}
		}
		return "", storage.ErrNotFound
	})
}

func (s *Scheduler) DeleteJob(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(false); err != nil {
		return err
	}
	_, err := s.write(ctx, func(tx *storage.Tx) error {
		tx.ProjectEvent = s.project(s.ready)
		return tx.DeleteJob(id)
	})
	return err
}

func (s *Scheduler) DeleteHistory(ctx context.Context, cutoff int64) (*pb.DeleteHistoryResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(false); err != nil {
		return nil, err
	}
	result := &pb.DeleteHistoryResponse{}
	_, err := s.write(ctx, func(tx *storage.Tx) error {
		tx.ProjectEvent = s.project(s.ready)
		var err error
		result.Deleted, result.Skipped, err = tx.DeleteHistory(cutoff)
		return err
	})
	return result, err
}

func (s *Scheduler) PruneEvents(ctx context.Context, age time.Duration, count int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(false); err != nil {
		return err
	}
	_, err := s.write(ctx, func(tx *storage.Tx) error {
		return tx.PruneEvents(time.Now().Add(-age).UnixNano(), count)
	})
	return err
}
