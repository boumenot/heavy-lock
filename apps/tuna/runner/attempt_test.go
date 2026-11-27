package runner

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/boumenot/tuna/apps/tuna/contract"
	pb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type blockedCancelAck struct {
	pb.RunnerServiceClient
	entered  chan struct{}
	finished chan struct{}
}

func (c *blockedCancelAck) AcknowledgeCancel(ctx context.Context, _ *pb.AcknowledgeCancelRequest, _ ...grpc.CallOption) (*pb.Job, error) {
	close(c.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (c *blockedCancelAck) ReportFinished(context.Context, *pb.ReportFinishedRequest, ...grpc.CallOption) (*pb.Job, error) {
	close(c.finished)
	return &pb.Job{}, nil
}

type blockedSeal struct {
	pb.RunnerServiceClient
	entered chan struct{}
}

func (c *blockedSeal) SealLaunch(ctx context.Context, _ *pb.SealLaunchRequest, _ ...grpc.CallOption) (*pb.Job, error) {
	close(c.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (*blockedSeal) ReportStarted(context.Context, *pb.ReportStartedRequest, ...grpc.CallOption) (*pb.Job, error) {
	return &pb.Job{}, nil
}

func (*blockedSeal) AcknowledgeCancel(context.Context, *pb.AcknowledgeCancelRequest, ...grpc.CallOption) (*pb.Job, error) {
	return &pb.Job{}, nil
}

type delayedClaimClient struct {
	pb.RunnerServiceClient
	entered  chan struct{}
	release  chan struct{}
	sealed   chan bool
	failed   chan bool
	ack      chan struct{}
	ackErr   error
	attempts atomic.Int32
}

func (c *delayedClaimClient) ClaimLaunch(_ context.Context, req *pb.ClaimLaunchRequest, _ ...grpc.CallOption) (*pb.ClaimLaunchResponse, error) {
	if c.attempts.Add(1) == 1 {
		close(c.entered)
	}
	<-c.release
	return &pb.ClaimLaunchResponse{Grant: &pb.Grant{GrantId: req.GrantId}}, nil
}

func (c *delayedClaimClient) SealLaunch(_ context.Context, req *pb.SealLaunchRequest, _ ...grpc.CallOption) (*pb.Job, error) {
	c.sealed <- req.GetNoCommandSpawned()
	return &pb.Job{}, nil
}

func (c *delayedClaimClient) ReportLaunchFailed(_ context.Context, req *pb.ReportLaunchFailedRequest, _ ...grpc.CallOption) (*pb.Job, error) {
	c.failed <- req.GetCancelled()
	return &pb.Job{}, nil
}

func (c *delayedClaimClient) AcknowledgeCancel(context.Context, *pb.AcknowledgeCancelRequest, ...grpc.CallOption) (*pb.Job, error) {
	close(c.ack)
	if c.ackErr != nil {
		return nil, c.ackErr
	}
	return &pb.Job{}, nil
}

type blockedNoLaunchSeal struct {
	pb.RunnerServiceClient
	entered chan struct{}
	ack     chan struct{}
}

type blockedFinish struct {
	pb.RunnerServiceClient
	events  chan *pb.JobEvent
	entered chan struct{}
	release chan struct{}
	ack     chan struct{}
	stopped chan struct{}
	reply   *pb.Job
}

type testJobStream struct {
	grpc.ServerStreamingClient[pb.JobEvent]
	ctx     context.Context
	events  <-chan *pb.JobEvent
	stopped chan struct{}
}

func (s *testJobStream) Recv() (*pb.JobEvent, error) {
	select {
	case event := <-s.events:
		return event, nil
	case <-s.ctx.Done():
		if s.stopped != nil {
			close(s.stopped)
		}
		return nil, s.ctx.Err()
	}
}

func (c *blockedFinish) WatchJob(ctx context.Context, _ *pb.WatchJobRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[pb.JobEvent], error) {
	return &testJobStream{ctx: ctx, events: c.events, stopped: c.stopped}, nil
}

func (c *blockedFinish) ReportFinished(ctx context.Context, _ *pb.ReportFinishedRequest, _ ...grpc.CallOption) (*pb.Job, error) {
	close(c.entered)
	select {
	case <-c.release:
		return c.reply, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *blockedFinish) AcknowledgeCancel(context.Context, *pb.AcknowledgeCancelRequest, ...grpc.CallOption) (*pb.Job, error) {
	close(c.ack)
	return &pb.Job{}, nil
}

func (c *blockedNoLaunchSeal) SealLaunch(ctx context.Context, _ *pb.SealLaunchRequest, _ ...grpc.CallOption) (*pb.Job, error) {
	close(c.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (c *blockedNoLaunchSeal) ReportLaunchFailed(context.Context, *pb.ReportLaunchFailedRequest, ...grpc.CallOption) (*pb.Job, error) {
	return &pb.Job{}, nil
}

func (c *blockedNoLaunchSeal) AcknowledgeCancel(context.Context, *pb.AcknowledgeCancelRequest, ...grpc.CallOption) (*pb.Job, error) {
	close(c.ack)
	return &pb.Job{}, nil
}

func TestCancelFencesLateClaimResponse(t *testing.T) {
	client := &delayedClaimClient{
		entered: make(chan struct{}), release: make(chan struct{}), sealed: make(chan bool, 1),
		failed: make(chan bool, 1), ack: make(chan struct{}),
	}
	a := &attempt{
		r: &runner{
			client: client, o: Options{ReconnectWindow: time.Second}, inc: &pb.RunnerIncarnation{},
			logf: func(string, ...any) {}, interrupt: context.Background(),
		},
		jobID: "job", grant: &pb.Grant{GrantId: "grant"},
	}
	a.launch()
	claims := a.claims
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(client.release) }) }
	t.Cleanup(release)
	select {
	case <-client.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("claim did not start")
	}
	a.launch()
	if a.claims != claims {
		t.Fatal("duplicate grant replaced the pending claim")
	}
	result, done := a.cancel(context.Background())
	if !done || result.code != ExitCancelled || !a.fenced {
		t.Fatalf("cancel did not fence the launch: %+v", result)
	}
	if !<-client.sealed || !<-client.failed {
		t.Fatal("cancel did not report no spawn")
	}
	a.waitReports(context.Background())
	if client.attempts.Load() != 1 {
		t.Fatalf("duplicate grant started %d claim calls", client.attempts.Load())
	}
	release()
	select {
	case claim := <-claims:
		if _, done := a.claimCompleted(context.Background(), claim); done || a.cmd != nil {
			t.Fatal("a late claim response launched a command")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("late claim response was not delivered")
	}
}

func TestCancelAcknowledgesWhileNoLaunchSealStalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &blockedNoLaunchSeal{entered: make(chan struct{}), ack: make(chan struct{})}
	a := &attempt{
		r: &runner{
			client: client, o: Options{ReconnectWindow: time.Minute}, inc: &pb.RunnerIncarnation{},
			logf: func(string, ...any) {},
		},
		jobID: "job", grant: &pb.Grant{GrantId: "grant"}, claims: make(chan claimResult, 1),
	}
	done := make(chan struct{})
	go func() {
		a.cancel(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
		a.waitReports(context.Background())
	}()
	select {
	case <-client.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("no-launch seal did not begin")
	}
	select {
	case <-client.ack:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation acknowledgment waited for the seal")
	}
}

func TestCancelAcknowledgesAfterChildExit(t *testing.T) {
	client := &delayedClaimClient{ack: make(chan struct{})}
	a := &attempt{
		r: &runner{
			client: client, o: Options{ReconnectWindow: time.Second}, inc: &pb.RunnerIncarnation{},
			logf: func(string, ...any) {},
		},
		jobID: "job", grant: &pb.Grant{GrantId: "grant"}, claimed: true, fenced: true, ran: true,
	}
	a.cancel(context.Background())
	a.waitReports(context.Background())
	select {
	case <-client.ack:
	default:
		t.Fatal("cancellation after child exit was not acknowledged")
	}
}

func TestCancelAcknowledgesWhileFinishReportIsBlocked(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		withSignal bool
		fromReply  bool
	}{
		{name: "event while finish is blocked"},
		{name: "event after signal", withSignal: true},
		{name: "cancellation in finish reply", fromReply: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			cmd := exec.Command("/usr/bin/true")
			if err := cmd.Run(); err != nil {
				t.Fatal(err)
			}
			client := &blockedFinish{
				events: make(chan *pb.JobEvent, 1), entered: make(chan struct{}),
				release: make(chan struct{}), ack: make(chan struct{}),
				reply: &pb.Job{},
			}
			if scenario.fromReply {
				at := int64(1)
				client.reply.CancelRequestedAt = contract.Timestamp(at)
			}
			interrupt, stopInterrupt := context.WithCancel(context.Background())
			defer stopInterrupt()
			a := &attempt{
				r: &runner{
					client: client, o: Options{ReconnectWindow: time.Minute},
					inc: &pb.RunnerIncarnation{}, logf: func(string, ...any) {},
					interrupt: interrupt,
				},
				jobID: "job", grant: &pb.Grant{GrantId: "grant"}, claimed: true, fenced: true,
				running: true, cmd: cmd, proc: &pb.ProcessIdentity{Pid: int64(cmd.Process.Pid), StartTime: contract.Timestamp(1)},
				exited: make(chan childExit, 1), heartbeat: defaultHeartbeat,
			}
			done := make(chan result, 1)
			var once sync.Once
			release := func() { once.Do(func() { close(client.release) }) }
			waited := false
			defer func() {
				release()
				if !waited {
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("runner did not finish after the report was released")
					}
				}
			}()
			go func() { done <- a.loop(context.Background()) }()
			a.exited <- childExit{state: cmd.ProcessState}
			select {
			case <-client.entered:
			case <-time.After(2 * time.Second):
				t.Fatal("finish report did not begin")
			}
			if scenario.withSignal {
				a.r.mu.Lock()
				a.r.signal, a.r.anySignal = syscall.SIGTERM, true
				a.r.mu.Unlock()
				stopInterrupt()
			}
			if scenario.fromReply {
				release()
			} else {
				client.events <- &pb.JobEvent{Event: &pb.JobEvent_CancelRequested{CancelRequested: &pb.CancelRequested{}}}
			}
			select {
			case <-client.ack:
			case <-time.After(time.Second):
				t.Fatal("cancellation was not acknowledged")
			}
			release()
			select {
			case result := <-done:
				waited = true
				if result.code != 0 || result.retryable {
					t.Fatalf("finished command result = %+v", result)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("runner did not finish after the report was released")
			}
		})
	}
}

func TestFinishedEventBeforeFinishReplyKeepsRetryEvidence(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		a := &attempt{
			r: &runner{logf: func(string, ...any) {}}, jobID: "job", ran: true,
			code: 7, finishPending: true, retryEligible: true,
		}
		code := int64(7)
		job := &pb.Job{State: pb.JobState_JOB_STATE_FINISHED, Outcome: &pb.Outcome{
			Kind: pb.OutcomeKind_OUTCOME_KIND_EXITED, ExitCode: &code,
		}}
		if cancelled {
			at := int64(1)
			job.CancelRequestedAt = contract.Timestamp(at)
		}
		if _, done := a.finished(job); done {
			t.Fatal("finished event ended the attempt before the report reply")
		}
		result, done := a.finishReported(nil)
		if !done || result.code != 7 || result.retryable == cancelled {
			t.Fatalf("cancelled=%v: finished result = %+v, done=%v", cancelled, result, done)
		}
	}
}

func TestWaitReportsAcknowledgesCancellationAfterChildExit(t *testing.T) {
	client := &delayedClaimClient{ack: make(chan struct{})}
	at := int64(1)
	a := &attempt{
		r: &runner{
			client: client, o: Options{ReconnectWindow: time.Second}, inc: &pb.RunnerIncarnation{},
			logf: func(string, ...any) {},
		},
		jobID: "job", grant: &pb.Grant{GrantId: "grant"}, claimed: true, fenced: true, ran: true,
		reports: make(chan reportResult, 2), pending: 2,
	}
	a.reports <- reportResult{name: "start", job: &pb.Job{CancelRequestedAt: contract.Timestamp(at)}}
	a.reports <- reportResult{name: "seal", job: &pb.Job{CancelRequestedAt: contract.Timestamp(at)}}
	a.waitReports(context.Background())
	select {
	case <-client.ack:
	default:
		t.Fatal("cancellation in a pending lifecycle reply was ignored")
	}
}

func TestWaitReportsLogsFailedCancellationAcknowledgment(t *testing.T) {
	client := &delayedClaimClient{ack: make(chan struct{}), ackErr: status.Error(codes.PermissionDenied, "denied")}
	var logs strings.Builder
	at := int64(1)
	a := &attempt{
		r: &runner{
			client: client, o: Options{ReconnectWindow: time.Second}, inc: &pb.RunnerIncarnation{},
			logf: func(format string, args ...any) { fmt.Fprintf(&logs, format, args...) },
		},
		jobID: "job", grant: &pb.Grant{GrantId: "grant"}, claimed: true, fenced: true, ran: true,
		reports: make(chan reportResult, 1), pending: 1,
	}
	a.reports <- reportResult{name: "start", job: &pb.Job{CancelRequestedAt: contract.Timestamp(at)}}
	a.waitReports(context.Background())
	if !strings.Contains(logs.String(), "cancellation acknowledgment report for job job not delivered:") ||
		!strings.Contains(logs.String(), "denied") {
		t.Fatalf("failed acknowledgment was not reported: %q", logs.String())
	}
}

func TestLoopDrainsCancellationReplyBeforeReturning(t *testing.T) {
	client := &blockedFinish{
		events: make(chan *pb.JobEvent), stopped: make(chan struct{}), ack: make(chan struct{}),
	}
	a := &attempt{
		r: &runner{
			client: client, o: Options{ReconnectWindow: time.Second}, inc: &pb.RunnerIncarnation{},
			logf: func(string, ...any) {}, interrupt: context.Background(),
		},
		jobID: "job", grant: &pb.Grant{GrantId: "grant"}, claimed: true, fenced: true,
		ran: true, code: 7, finishPending: true, heartbeat: defaultHeartbeat,
	}
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseReport := func() { releaseOnce.Do(func() { close(release) }) }
	a.report("start", func() (*pb.Job, error) {
		<-release
		at := int64(1)
		return &pb.Job{CancelRequestedAt: contract.Timestamp(at)}, nil
	})
	a.report("finish", func() (*pb.Job, error) { return &pb.Job{}, nil })
	done := make(chan result, 1)
	go func() { done <- a.loop(context.Background()) }()
	waited := false
	defer func() {
		releaseReport()
		if !waited {
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Error("runner did not exit after releasing the report")
			}
		}
	}()

	select {
	case <-client.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not begin draining the remaining report")
	}
	releaseReport()
	select {
	case got := <-done:
		waited = true
		if got.code != 7 || got.retryable || a.pending != 0 {
			t.Fatalf("drained result = %+v, pending reports = %d", got, a.pending)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not finish draining reports")
	}
	select {
	case <-client.ack:
	default:
		t.Fatal("cancellation in the drained report was ignored")
	}
}

func TestCancelStopsChildWhileAcknowledgmentIsBlocked(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &blockedCancelAck{entered: make(chan struct{}), finished: make(chan struct{})}
	cmd := exec.Command("/bin/sleep", "30")
	start := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	exited := make(chan childExit, 1)
	done := make(chan struct{})
	a := &attempt{
		r: &runner{
			client:          client,
			o:               Options{ReconnectWindow: time.Minute},
			inc:             &pb.RunnerIncarnation{},
			logf:            func(string, ...any) {},
			child:           cmd.Process,
			childSignalable: true,
			interrupt:       context.Background(),
		},
		jobID:   "job",
		grant:   &pb.Grant{GrantId: "grant"},
		running: true,
		cmd:     cmd,
		proc:    &pb.ProcessIdentity{Pid: int64(cmd.Process.Pid), StartTime: contract.Timestamp(1)},
	}
	go func() {
		exited <- waitForCommand(cmd, start, func() { a.r.disarmChild(cmd.Process) }, a.r.logf)
	}()
	go func() {
		a.cancel(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		select {
		case <-exited:
		default:
			_ = a.r.signalChild(cmd.Process, syscall.SIGTERM)
			<-exited
		}
		<-done
		a.waitReports(context.Background())
	}()
	select {
	case <-client.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not attempt acknowledgment")
	}
	select {
	case exit := <-exited:
		exited <- exit
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("cancellation blocked the runner loop on acknowledgment")
		}
		if _, done := a.childExited(exit); done {
			t.Fatal("finished command completed before its report")
		}
		select {
		case <-client.finished:
		case <-time.After(2 * time.Second):
			t.Fatal("finish report waited for cancellation acknowledgment")
		}
		select {
		case report := <-a.reports:
			a.finishReport(report)
			if _, done := a.finishReported(report.err); !done {
				t.Fatal("finished command did not complete after its report")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("finish report did not complete")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("child still running while cancellation acknowledgment is blocked")
	}
}

func TestSpawnDoesNotBlockCancellationWhileSealReportStalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &blockedSeal{entered: make(chan struct{})}
	a := &attempt{
		r: &runner{
			client: client,
			o:      Options{Command: []string{"/bin/sleep", "30"}, ReconnectWindow: time.Minute},
			inc:    &pb.RunnerIncarnation{},
			logf:   func(string, ...any) {},
		},
		jobID:   "job",
		grant:   &pb.Grant{GrantId: "grant"},
		exited:  make(chan childExit, 1),
		claimed: true,
	}
	done := make(chan struct{})
	go func() {
		a.spawn(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
		select {
		case <-a.exited:
		default:
			_ = a.r.signalChild(a.cmd.Process, syscall.SIGTERM)
			<-a.exited
		}
		a.waitReports(context.Background())
	}()
	select {
	case <-client.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("seal report did not begin")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("spawn blocked the cancellation loop on the seal report")
	}
	a.cancel(ctx)
	select {
	case state := <-a.exited:
		a.exited <- state
	case <-time.After(2 * time.Second):
		t.Fatal("command did not exit after cancellation")
	}
}
