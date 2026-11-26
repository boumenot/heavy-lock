package broker

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/boumenot/heavy-lock/apps/tuna/contract"
	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"github.com/boumenot/heavy-lock/apps/tuna/scheduler"
	"github.com/boumenot/heavy-lock/apps/tuna/storage"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

// DefaultHeartbeatInterval is the initial runner stream heartbeat period.
const DefaultHeartbeatInterval = 5 * time.Second

// streamBuffer bounds the live events queued for one runner stream. A stream
// that falls further behind is closed with EVENTS_EXPIRED and must re-watch.
const streamBuffer = 1024

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type RunnerOptions struct {
	HeartbeatInterval time.Duration
}

// Runner serves RunnerService over the single shared scheduler. Streams
// subscribe to the hub that the scheduler publishes to.
type Runner struct {
	pb.UnimplementedRunnerServiceServer
	sched     *scheduler.Scheduler
	hub       *Hub
	heartbeat time.Duration
}

func NewRunner(s *scheduler.Scheduler, hub *Hub, o RunnerOptions) (*Runner, error) {
	if s == nil || hub == nil {
		return nil, errors.New("scheduler and event hub are required")
	}
	if o.HeartbeatInterval <= 0 {
		o.HeartbeatInterval = DefaultHeartbeatInterval
	}
	return &Runner{sched: s, hub: hub, heartbeat: o.HeartbeatInterval}, nil
}

func validRunner(r *pb.RunnerIncarnation) error {
	if r == nil || !uuidV4.MatchString(r.IncarnationId) {
		return invalid("runner.incarnation_id", "a lowercase canonical UUIDv4 runner incarnation is required")
	}
	if r.Process == nil || r.Process.Pid <= 0 || r.Process.StartTimeUnixNs <= 0 {
		return invalid("runner.process", "the runner process identity is required")
	}
	return nil
}

func validSubmit(req *pb.SubmitJobRequest) error {
	switch {
	case !uuidV4.MatchString(req.GetIdempotencyKey()):
		return invalid("idempotency_key", "a lowercase canonical UUIDv4 idempotency key is required")
	case req.GetCommand() == "":
		return invalid("command", "a command is required")
	case req.GetWeight() == 0:
		return invalid("weight", "a positive weight is required")
	case req.RetryOfJobId != nil && *req.RetryOfJobId == "":
		return invalid("retry_of_job_id", "the retried job ID must not be empty")
	}
	return validRunner(req.GetRunner())
}

func validGrant(jobID, grantID string, runner *pb.RunnerIncarnation) error {
	if jobID == "" {
		return invalid("job_id", "a job ID is required")
	}
	if grantID == "" {
		return invalid("grant_id", "a grant ID is required")
	}
	return validRunner(runner)
}

func validProcess(field string, p *pb.ProcessIdentity) error {
	if p == nil || p.Pid <= 0 || p.StartTimeUnixNs <= 0 {
		return invalid(field, "a process identity with PID and start time is required")
	}
	return nil
}

func validOutcome(o *pb.Outcome) error {
	switch {
	case o == nil:
		return invalid("outcome", "an outcome is required")
	case o.Kind == pb.OutcomeKind_OUTCOME_KIND_EXITED:
		if o.ExitCode == nil || o.SignalNumber != nil || o.SignalName != nil {
			return invalid("outcome", "an exited outcome needs an exit code and no signal")
		}
		if *o.ExitCode < -2147483648 || *o.ExitCode > 4294967295 {
			return invalid("outcome.exit_code", "exit code is outside the supported native range")
		}
	case o.Kind == pb.OutcomeKind_OUTCOME_KIND_SIGNALED:
		if o.SignalNumber == nil || *o.SignalNumber <= 0 {
			return invalid("outcome.signal_number", "a signaled outcome needs a positive signal number")
		}
	default:
		return invalid("outcome.kind", "a runner reports only exited or signaled outcomes")
	}
	return nil
}

// lifecycleError gives identity and transition failures of a grant-bound
// call a precise reason. The grant ID and incarnation are immutable once
// reserved, so a read after the failure classifies it reliably.
func (b *Runner) lifecycleError(ctx context.Context, id, grant string, runner *pb.RunnerIncarnation, err error) error {
	if errors.Is(err, scheduler.ErrGrantFenced) ||
		!errors.Is(err, storage.ErrIdentity) && !errors.Is(err, storage.ErrTransition) {
		return scheduler.ContractError(err)
	}
	job, jerr := b.sched.Job(context.WithoutCancel(ctx), id)
	switch {
	case jerr != nil:
		return scheduler.ContractError(jerr)
	case job.Grant == nil:
		return contract.Error(codes.FailedPrecondition, pb.ErrorReason_JOB_NOT_WAITING, "job has no grant: "+err.Error(), nil)
	case job.Grant.GrantId != grant:
		return contract.Error(codes.FailedPrecondition, pb.ErrorReason_GRANT_MISMATCH, "grant does not match the job's grant", nil)
	case !proto.Equal(job.Grant.Runner, runner):
		return contract.Error(codes.FailedPrecondition, pb.ErrorReason_CLAIM_MISMATCH, "grant belongs to a different runner incarnation", nil)
	default:
		return contract.Error(codes.FailedPrecondition, pb.ErrorReason_REPORT_CONFLICT, err.Error(), nil)
	}
}

func (b *Runner) SubmitJob(ctx context.Context, req *pb.SubmitJobRequest) (*pb.SubmitJobResponse, error) {
	if err := validSubmit(req); err != nil {
		return nil, err
	}
	result, _, err := b.sched.Submit(ctx, req)
	if err != nil {
		return nil, scheduler.ContractError(err)
	}
	return &pb.SubmitJobResponse{Job: result.Job, Created: result.Created}, nil
}

func (b *Runner) TrySubmitAndWatch(req *pb.SubmitJobRequest, stream grpc.ServerStreamingServer[pb.JobEvent]) error {
	if err := validSubmit(req); err != nil {
		return err
	}
	ctx := stream.Context()
	sub := b.hub.Subscribe(streamBuffer)
	defer sub.Close()
	reg, job, _, err := b.sched.TrySubmitAndWatch(ctx, req)
	if err != nil {
		return scheduler.ContractError(err)
	}
	return b.serve(ctx, stream, sub, reg, job)
}

func (b *Runner) WatchJob(req *pb.WatchJobRequest, stream grpc.ServerStreamingServer[pb.JobEvent]) error {
	if req.GetJobId() == "" {
		return invalid("job_id", "a job ID is required")
	}
	if err := validRunner(req.GetRunner()); err != nil {
		return err
	}
	ctx := stream.Context()
	sub := b.hub.Subscribe(streamBuffer)
	defer sub.Close()
	reg, job, _, err := b.sched.Watch(ctx, req.JobId, req.Runner)
	if reg == nil && err != nil {
		return b.watchError(err)
	}
	// A registration is valid even if the following admission pass failed;
	// admission is retried by later scheduler activity.
	return b.serve(ctx, stream, sub, reg, job)
}

func (b *Runner) watchError(err error) error {
	if !errors.Is(err, storage.ErrIdentity) {
		return scheduler.ContractError(err)
	}
	return contract.Error(codes.FailedPrecondition, pb.ErrorReason_CLAIM_MISMATCH,
		"job belongs to a different runner incarnation", nil)
}

// serve sends the snapshot and then per-job transitions derived from live
// committed events newer than the snapshot. It never releases capacity: a
// closed stream only disconnects its registration.
func (b *Runner) serve(ctx context.Context, stream grpc.ServerStreamingServer[pb.JobEvent], sub *Subscription, reg *scheduler.Registration, job *pb.Job) error {
	seq := b.sched.EventSeq()
	if reg != nil {
		seq = reg.Seq
		defer func() { _, _ = b.sched.Disconnect(context.WithoutCancel(ctx), reg) }()
	}
	now := func() int64 { return time.Now().UnixNano() }
	send := func(e *pb.JobEvent) error {
		e.EventSeq, e.AtUnixNs = seq, now()
		return stream.Send(e)
	}
	if err := send(&pb.JobEvent{Event: &pb.JobEvent_Snapshot{Snapshot: job}}); err != nil {
		return err
	}
	if job.State == pb.JobState_JOB_STATE_FINISHED {
		return send(&pb.JobEvent{Event: &pb.JobEvent_Finished{Finished: job}})
	}
	t := newTracker(job)
	ticker := time.NewTicker(b.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-sub.Overflowed():
			return contract.Error(codes.OutOfRange, pb.ErrorReason_EVENTS_EXPIRED,
				"runner stream fell behind; watch the job again for a fresh snapshot", nil)
		case <-ticker.C:
			if reg != nil && !b.sched.Current(reg) {
				return nil // superseded by a newer stream for the same job
			}
			if err := send(&pb.JobEvent{Event: &pb.JobEvent_Heartbeat{Heartbeat: &pb.Heartbeat{HeartbeatIntervalMs: b.heartbeat.Milliseconds()}}}); err != nil {
				return err
			}
		case e := <-sub.Events():
			if e.EventSeq <= seq {
				continue
			}
			seq = e.EventSeq
			if e.GetJobDeleted() == job.JobId {
				return contract.Error(codes.NotFound, pb.ErrorReason_JOB_NOT_FOUND, "job history was deleted", nil)
			}
			out, done := t.apply(e, job.JobId)
			for _, o := range out {
				o.EventSeq, o.AtUnixNs = seq, e.AtUnixNs
				if err := stream.Send(o); err != nil {
					return err
				}
			}
			if done {
				return nil
			}
		}
	}
}

// tracker derives a runner's per-job events from broker-wide events. Each
// transition is sent once; the snapshot already carries the state it shows.
type tracker struct {
	granted, cancelled bool
	position           *pb.QueuePositionChanged
}

func newTracker(j *pb.Job) *tracker {
	t := &tracker{granted: j.Grant != nil, cancelled: j.CancelRequestedAtUnixNs != nil}
	if j.QueuePosition != nil {
		t.position = &pb.QueuePositionChanged{Position: *j.QueuePosition}
	}
	return t
}

func (t *tracker) apply(e *pb.BrokerEvent, id string) ([]*pb.JobEvent, bool) {
	var out []*pb.JobEvent
	if q := e.GetQueueChanged(); q != nil {
		for i, j := range q.Jobs {
			if j.JobId != id {
				continue
			}
			p := &pb.QueuePositionChanged{Position: uint32(i), QueueLength: uint32(len(q.Jobs)), HeadBlockedReason: q.HeadBlockedReason}
			if !proto.Equal(p, t.position) {
				t.position = p
				out = append(out, &pb.JobEvent{Event: &pb.JobEvent_QueuePositionChanged{QueuePositionChanged: p}})
			}
		}
		return out, false
	}
	j := e.GetJobChanged()
	if j == nil || j.JobId != id {
		return nil, false
	}
	if !t.granted && j.Grant != nil {
		t.granted = true
		t.position = nil
		out = append(out, &pb.JobEvent{Event: &pb.JobEvent_Granted{Granted: j.Grant}})
	}
	if !t.cancelled && j.CancelRequestedAtUnixNs != nil && j.State != pb.JobState_JOB_STATE_FINISHED {
		t.cancelled = true
		running := j.State == pb.JobState_JOB_STATE_RUNNING || j.State == pb.JobState_JOB_STATE_FINISHING
		out = append(out, &pb.JobEvent{Event: &pb.JobEvent_CancelRequested{CancelRequested: &pb.CancelRequested{TerminateRunning: running, Reason: j.CancelReason}}})
	}
	if j.State == pb.JobState_JOB_STATE_FINISHED {
		return append(out, &pb.JobEvent{Event: &pb.JobEvent_Finished{Finished: j}}), true
	}
	return out, false
}

func (b *Runner) RejoinJob(ctx context.Context, req *pb.RejoinJobRequest) (*pb.Job, error) {
	if req.GetJobId() == "" {
		return nil, invalid("job_id", "a job ID is required")
	}
	if err := validRunner(req.GetRunner()); err != nil {
		return nil, err
	}
	job, err := b.sched.Rejoin(ctx, req.JobId, req.Runner)
	if errors.Is(err, storage.ErrIdentity) {
		return nil, contract.Error(codes.FailedPrecondition, pb.ErrorReason_CLAIM_MISMATCH, "job belongs to a different runner incarnation", nil)
	}
	return job, scheduler.ContractError(err)
}

func (b *Runner) ClaimLaunch(ctx context.Context, req *pb.ClaimLaunchRequest) (*pb.ClaimLaunchResponse, error) {
	if err := validGrant(req.GetJobId(), req.GetGrantId(), req.GetRunner()); err != nil {
		return nil, err
	}
	job, already, _, err := b.sched.Claim(ctx, req.JobId, req.GrantId, req.Runner)
	if err != nil {
		return nil, b.lifecycleError(ctx, req.JobId, req.GrantId, req.Runner, err)
	}
	return &pb.ClaimLaunchResponse{Grant: job.Grant, AlreadyClaimed: already}, nil
}

func (b *Runner) SealLaunch(ctx context.Context, req *pb.SealLaunchRequest) (*pb.Job, error) {
	if err := validGrant(req.GetJobId(), req.GetGrantId(), req.GetRunner()); err != nil {
		return nil, err
	}
	job, _, err := b.sched.Seal(ctx, req.JobId, req.GrantId, req.Runner, req.NoCommandSpawned)
	return b.result(ctx, req.JobId, req.GrantId, req.Runner, job, err)
}

func (b *Runner) ReportStarted(ctx context.Context, req *pb.ReportStartedRequest) (*pb.Job, error) {
	if err := validGrant(req.GetJobId(), req.GetGrantId(), req.GetRunner()); err != nil {
		return nil, err
	}
	if err := validProcess("process", req.Process); err != nil {
		return nil, err
	}
	job, _, err := b.sched.ReportStarted(ctx, req.JobId, req.GrantId, req.Runner, req.Process)
	return b.result(ctx, req.JobId, req.GrantId, req.Runner, job, err)
}

func (b *Runner) ReportLaunchFailed(ctx context.Context, req *pb.ReportLaunchFailedRequest) (*pb.Job, error) {
	if err := validGrant(req.GetJobId(), req.GetGrantId(), req.GetRunner()); err != nil {
		return nil, err
	}
	if req.Error == "" {
		return nil, invalid("error", "a launch failure detail is required")
	}
	job, _, err := b.sched.ReportLaunchFailed(ctx, req.JobId, req.GrantId, req.Runner, req.Error, req.Cancelled)
	return b.result(ctx, req.JobId, req.GrantId, req.Runner, job, err)
}

func (b *Runner) AcknowledgeCancel(ctx context.Context, req *pb.AcknowledgeCancelRequest) (*pb.Job, error) {
	if err := validGrant(req.GetJobId(), req.GetGrantId(), req.GetRunner()); err != nil {
		return nil, err
	}
	job, _, err := b.sched.AcknowledgeCancel(ctx, req.JobId, req.GrantId, req.Runner)
	return b.result(ctx, req.JobId, req.GrantId, req.Runner, job, err)
}

func (b *Runner) ReportCancelFailed(ctx context.Context, req *pb.ReportCancelFailedRequest) (*pb.Job, error) {
	if err := validGrant(req.GetJobId(), req.GetGrantId(), req.GetRunner()); err != nil {
		return nil, err
	}
	if req.Error == "" {
		return nil, invalid("error", "a cancellation failure detail is required")
	}
	job, _, err := b.sched.ReportCancelFailed(ctx, req.JobId, req.GrantId, req.Runner, req.Error)
	return b.result(ctx, req.JobId, req.GrantId, req.Runner, job, err)
}

func (b *Runner) ReportFinished(ctx context.Context, req *pb.ReportFinishedRequest) (*pb.Job, error) {
	if err := validGrant(req.GetJobId(), req.GetGrantId(), req.GetRunner()); err != nil {
		return nil, err
	}
	if err := validProcess("process", req.Process); err != nil {
		return nil, err
	}
	if err := validOutcome(req.Outcome); err != nil {
		return nil, err
	}
	job, _, err := b.sched.ReportFinished(ctx, req.JobId, req.GrantId, req.Runner, req.Process, req.Outcome)
	return b.result(ctx, req.JobId, req.GrantId, req.Runner, job, err)
}

func (b *Runner) result(ctx context.Context, id, grant string, runner *pb.RunnerIncarnation, job *pb.Job, err error) (*pb.Job, error) {
	if err != nil && job == nil {
		return nil, b.lifecycleError(ctx, id, grant, runner, err)
	}
	// A non-nil job means the report committed; a failed admission pass
	// after a release is retried by later scheduler activity.
	return job, nil
}
