// Package broker adapts the single scheduler to the shared control contract.
package broker

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/boumenot/tuna/apps/tuna/contract"
	pb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
	"github.com/boumenot/tuna/apps/tuna/scheduler"
	"github.com/boumenot/tuna/apps/tuna/storage"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
)

type Options struct {
	Hub               *Hub
	ServiceVersion    string
	EventMaxAge       time.Duration
	EventMaxCount     int64
	HeartbeatInterval time.Duration
}

type Control struct {
	pb.UnimplementedControlServiceServer
	s       *scheduler.Scheduler
	store   *storage.Store
	opts    Options
	started int64
}

func NewControl(s *scheduler.Scheduler, store *storage.Store, opts Options) (*Control, error) {
	if s == nil || store == nil || opts.Hub == nil || strings.TrimSpace(opts.ServiceVersion) == "" {
		return nil, errors.New("scheduler, store, shared publication hub and service version are required")
	}
	if opts.EventMaxAge == 0 {
		opts.EventMaxAge = 24 * time.Hour
	}
	if opts.EventMaxCount == 0 {
		opts.EventMaxCount = 100000
	}
	if opts.HeartbeatInterval == 0 {
		opts.HeartbeatInterval = 5 * time.Second
	}
	if opts.EventMaxAge < 0 || opts.EventMaxCount < 1 || opts.HeartbeatInterval < 0 {
		return nil, errors.New("event retention and heartbeat must be positive")
	}
	return &Control{s: s, store: store, opts: opts, started: time.Now().UnixNano()}, nil
}

var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func invalid(field, message string) error {
	return contract.Error(codes.InvalidArgument, pb.ErrorReason_INVALID_ARGUMENT_FIELD, message, map[string]string{"field": field})
}

func validID(id, field string) error {
	if !uuid.MatchString(id) {
		return invalid(field, "expected a canonical lowercase UUIDv4")
	}
	return nil
}

func controlError(err error) error {
	if errors.Is(err, storage.ErrEventsExpired) {
		return contract.Error(codes.OutOfRange, pb.ErrorReason_EVENTS_EXPIRED, "event replay expired; refresh with after_event_seq=0", nil)
	}
	if errors.Is(err, storage.ErrPageToken) {
		return invalid("page_token", err.Error())
	}
	if errors.Is(err, storage.ErrUnreconciled) {
		return contract.Error(codes.FailedPrecondition, pb.ErrorReason_PROCESSES_UNRECONCILED, err.Error(), nil)
	}
	return scheduler.ContractError(err)
}

func (c *Control) GetHealth(ctx context.Context, _ *pb.GetHealthRequest) (*pb.Health, error) {
	h := &pb.Health{Live: true, ServiceVersion: c.opts.ServiceVersion, StartedAt: contract.Timestamp(c.started),
		SchemaVersion: storage.SchemaVersion, ServiceIdentity: contract.ServiceIdentity}
	snapshot, err := c.s.Snapshot(ctx)
	if errors.Is(err, storage.ErrUnavailable) {
		detail := err.Error()
		h.StorageDetail = &detail
		return h, nil
	}
	if err != nil {
		return nil, controlError(err)
	}
	h.Ready = c.s.Ready() && !snapshot.Status.Recovering
	if !h.Ready {
		detail := "broker is recovering"
		h.RecoveryDetail = &detail
	}
	return h, nil
}

func (c *Control) GetStatus(ctx context.Context, _ *pb.GetStatusRequest) (*pb.BrokerStatus, error) {
	snapshot, err := c.s.Snapshot(ctx)
	if errors.Is(err, storage.ErrUnavailable) {
		// Durable mode and counts are unknown; only configuration is trusted.
		config := c.s.Config()
		return &pb.BrokerStatus{Capacity: config.Capacity, BackfillLimit: config.BackfillLimit,
			Recovering: !c.s.Ready(), StorageUnavailable: true,
			HeadBlockedReason: pb.BlockedReason_BLOCKED_REASON_STORAGE_UNAVAILABLE,
			QueueState:        pb.QueueState_QUEUE_STATE_ERROR}, nil
	}
	if err != nil {
		return nil, controlError(err)
	}
	return snapshot.Status, nil
}

func (c *Control) GetQueue(ctx context.Context, _ *pb.GetQueueRequest) (*pb.Queue, error) {
	snapshot, err := c.s.Snapshot(ctx)
	if err != nil {
		return nil, controlError(err)
	}
	return snapshot.Queue, nil
}

func (c *Control) GetJob(ctx context.Context, req *pb.GetJobRequest) (*pb.Job, error) {
	if err := validID(req.GetJobId(), "job_id"); err != nil {
		return nil, err
	}
	j, err := c.s.Job(ctx, req.JobId)
	return j, controlError(err)
}

func (c *Control) GetSubmission(ctx context.Context, req *pb.GetSubmissionRequest) (*pb.Job, error) {
	if err := validID(req.GetIdempotencyKey(), "idempotency_key"); err != nil {
		return nil, err
	}
	j, err := c.s.Submission(ctx, req.IdempotencyKey)
	return j, controlError(err)
}

func (c *Control) ListJobs(ctx context.Context, req *pb.ListJobsRequest) (*pb.ListJobsResponse, error) {
	if req == nil {
		return nil, invalid("request", "request is required")
	}
	if req.PageSize > 500 {
		return nil, invalid("page_size", "maximum page size is 500")
	}
	for _, state := range req.States {
		if state < pb.JobState_JOB_STATE_QUEUED || state > pb.JobState_JOB_STATE_FINISHED {
			return nil, invalid("states", "expected a nonzero JobState")
		}
	}
	var after, before int64
	var err error
	if req.QueuedAfter != nil {
		if after, err = contract.UnixNano(req.QueuedAfter); err != nil {
			return nil, invalid("queued_after", err.Error())
		}
	}
	if req.QueuedBefore != nil {
		if before, err = contract.UnixNano(req.QueuedBefore); err != nil {
			return nil, invalid("queued_before", err.Error())
		}
	}
	if req.QueuedAfter != nil && req.QueuedBefore != nil && after >= before {
		return nil, invalid("queued_before", "end must be after start")
	}
	result, err := c.s.ListJobs(ctx, req)
	return result, controlError(err)
}

func (c *Control) PauseQueue(ctx context.Context, _ *pb.PauseQueueRequest) (*pb.BrokerStatus, error) {
	if _, err := c.s.SetQueueMode(ctx, pb.QueueMode_QUEUE_MODE_PAUSED); err != nil {
		return nil, controlError(err)
	}
	return c.GetStatus(ctx, &pb.GetStatusRequest{})
}

func (c *Control) ResumeQueue(ctx context.Context, _ *pb.ResumeQueueRequest) (*pb.BrokerStatus, error) {
	if _, err := c.s.SetQueueMode(ctx, pb.QueueMode_QUEUE_MODE_ENABLED); err != nil {
		return nil, controlError(err)
	}
	return c.GetStatus(ctx, &pb.GetStatusRequest{})
}

func (c *Control) MoveJob(ctx context.Context, req *pb.MoveJobRequest) (*pb.Queue, error) {
	if err := validID(req.GetJobId(), "job_id"); err != nil {
		return nil, err
	}
	var err error
	switch target := req.Target.(type) {
	case *pb.MoveJobRequest_ToFront:
		if !target.ToFront {
			return nil, invalid("to_front", "must be true")
		}
		_, err = c.s.MoveToFront(ctx, req.JobId)
	case *pb.MoveJobRequest_ToBack:
		if !target.ToBack {
			return nil, invalid("to_back", "must be true")
		}
		_, err = c.s.MoveBefore(ctx, req.JobId, "")
	case *pb.MoveJobRequest_BeforeJobId:
		if err := validID(target.BeforeJobId, "before_job_id"); err != nil {
			return nil, err
		}
		_, err = c.s.MoveBefore(ctx, req.JobId, target.BeforeJobId)
	case *pb.MoveJobRequest_AfterJobId:
		if err := validID(target.AfterJobId, "after_job_id"); err != nil {
			return nil, err
		}
		_, err = c.s.MoveAfter(ctx, req.JobId, target.AfterJobId)
	default:
		return nil, invalid("target", "one reorder target is required")
	}
	if err != nil {
		return nil, controlError(err)
	}
	return c.GetQueue(ctx, &pb.GetQueueRequest{})
}

func (c *Control) ForceStartJob(ctx context.Context, req *pb.ForceStartJobRequest) (*pb.Job, error) {
	if err := validID(req.GetJobId(), "job_id"); err != nil {
		return nil, err
	}
	j, _, err := c.s.ForceStart(ctx, req.JobId)
	return j, controlError(err)
}

func (c *Control) CancelJob(ctx context.Context, req *pb.CancelJobRequest) (*pb.Job, error) {
	if err := validID(req.GetJobId(), "job_id"); err != nil {
		return nil, err
	}
	if _, err := c.s.Cancel(ctx, req.JobId, req.Reason); err != nil {
		if errors.Is(err, storage.ErrTransition) {
			return nil, contract.Error(codes.FailedPrecondition, pb.ErrorReason_JOB_TERMINAL, err.Error(), nil)
		}
		return nil, controlError(err)
	}
	return c.GetJob(ctx, &pb.GetJobRequest{JobId: req.JobId})
}

func (c *Control) DeleteJob(ctx context.Context, req *pb.DeleteJobRequest) (*pb.DeleteJobResponse, error) {
	if err := validID(req.GetJobId(), "job_id"); err != nil {
		return nil, err
	}
	err := c.s.DeleteJob(ctx, req.JobId)
	if errors.Is(err, storage.ErrTransition) {
		return nil, contract.Error(codes.FailedPrecondition, pb.ErrorReason_JOB_NOT_TERMINAL, err.Error(), nil)
	}
	if err != nil {
		return nil, controlError(err)
	}
	return &pb.DeleteJobResponse{}, nil
}

func (c *Control) DeleteHistory(ctx context.Context, req *pb.DeleteHistoryRequest) (*pb.DeleteHistoryResponse, error) {
	if req == nil {
		return nil, invalid("request", "request is required")
	}
	cutoff, err := contract.UnixNano(req.FinishedBefore)
	if err != nil {
		return nil, invalid("finished_before", err.Error())
	}
	result, err := c.s.DeleteHistory(ctx, cutoff)
	return result, controlError(err)
}

// Maintain is run by the broker's existing maintenance loop; retention is
// independent of listeners and never deletes job history.
func (c *Control) Maintain(ctx context.Context) error {
	return controlError(c.s.PruneEvents(ctx, c.opts.EventMaxAge, c.opts.EventMaxCount))
}

func (c *Control) WatchEvents(req *pb.WatchEventsRequest, stream grpc.ServerStreamingServer[pb.BrokerEvent]) error {
	cursor := req.GetAfterEventSeq()
	if cursor < 0 {
		return invalid("after_event_seq", "must be nonnegative")
	}
	subscription := c.opts.Hub.Subscribe(500)
	defer subscription.Close()
	if cursor == 0 {
		event, err := c.s.SnapshotEvent(stream.Context())
		if err != nil {
			return controlError(err)
		}
		if err := stream.Send(event); err != nil {
			return err
		}
		cursor = event.EventSeq
	}
	heartbeat := time.NewTicker(c.opts.HeartbeatInterval)
	defer heartbeat.Stop()
	for {
		var events []*pb.BrokerEvent
		err := c.store.View(stream.Context(), func(r *storage.Reader) error {
			var err error
			events, err = r.Events(cursor, 500)
			return err
		})
		if err != nil {
			return controlError(err)
		}
		for _, event := range events {
			if err := stream.Send(event); err != nil {
				return err
			}
			cursor = event.EventSeq
		}
		if len(events) == 500 {
			continue
		}
		break
	}
	for {
		select {
		case <-subscription.Overflowed():
			return controlError(storage.ErrEventsExpired)
		default:
		}
		// A failed write publishes no event, so end the stream instead of
		// heartbeating stale live state; clients reconnect and read Health.
		if failure := c.store.Failure(); failure != nil {
			return controlError(errors.Join(storage.ErrUnavailable, failure))
		}
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-subscription.Overflowed():
			return controlError(storage.ErrEventsExpired)
		case event := <-subscription.Events():
			if event.EventSeq <= cursor {
				continue
			}
			if event.EventSeq != cursor+1 {
				return controlError(storage.ErrEventsExpired)
			}
			if err := stream.Send(event); err != nil {
				return err
			}
			cursor = event.EventSeq
		case <-heartbeat.C:
			if err := stream.Send(&pb.BrokerEvent{EventSeq: cursor, At: contract.Timestamp(time.Now().UnixNano()),
				Event: &pb.BrokerEvent_Heartbeat{Heartbeat: &pb.Heartbeat{HeartbeatInterval: durationpb.New(c.opts.HeartbeatInterval)}}}); err != nil {
				return err
			}
		}
	}
}
