package runner

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/boumenot/heavy-lock/apps/tuna/contract"
	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"github.com/boumenot/heavy-lock/apps/tuna/procid"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"
)

type streamMsg struct {
	ev  *pb.JobEvent
	err error
}

type reportResult struct {
	name string
	job  *pb.Job
	err  error
}

type claimResult struct {
	response *pb.ClaimLaunchResponse
	err      error
}

// attempt is one submission and, at most, one spawn of its command. All of
// its state is owned by the loop goroutine, so the spawn and every decision
// not to spawn are serialized.
type attempt struct {
	r      *runner
	req    *pb.SubmitJobRequest
	noWait bool

	jobID string
	grant *pb.Grant
	// fenced is set before any spawn and when spawning is ruled out, so no
	// further spawn can happen under this attempt.
	fenced     bool
	claimed    bool
	cancelSeen bool
	running    bool
	noWatch    bool
	cmd        *exec.Cmd
	proc       *pb.ProcessIdentity
	exited     chan childExit
	reports    chan reportResult
	pending    int
	claims     chan claimResult
	stopClaim  context.CancelFunc

	ran                    bool // the command was spawned and has exited
	code                   int
	finishPending          bool
	retryEligible          bool
	finishedWhileReporting *pb.Job
	awaitFinal             time.Time // non-zero while waiting for release before a retry
	heartbeat              time.Duration
	position               *uint32
}

func (r *runner) attempt(ctx context.Context, retryOf string, noWait bool) result {
	a := &attempt{
		r:         r,
		req:       r.submitRequest(retryOf),
		noWait:    noWait,
		exited:    make(chan childExit, 1),
		heartbeat: defaultHeartbeat,
	}
	if !noWait {
		// Retransmission reuses the key, so an ambiguous reply cannot create
		// a second job.
		resp, err := retry(r.interrupt, r.o.ReconnectWindow, func(c context.Context) (*pb.SubmitJobResponse, error) {
			return r.client.SubmitJob(c, a.req)
		})
		if err != nil {
			return a.submitFailed(err)
		}
		a.jobID = resp.GetJob().GetJobId()
		r.logf("submitted job %s", a.jobID)
	}
	return a.loop(ctx)
}

func (a *attempt) submitFailed(err error) result {
	if sig, ok := a.r.interrupted(); ok {
		a.r.logf("interrupted while submitting (key %s)", a.req.IdempotencyKey)
		return result{code: 128 + int(sig)}
	}
	switch reason(err) {
	case pb.ErrorReason_ADMISSION_BUSY:
		a.r.logf("broker is busy; not admitted (--no-wait)")
		return result{code: ExitBusy}
	case pb.ErrorReason_RETRY_NOT_ELIGIBLE:
		a.r.logf("retry refused: %s", describe(err))
		return result{code: ExitSoftware, jobID: a.req.GetRetryOfJobId()}
	}
	if transient(err) {
		a.r.logf("submission outcome unknown (idempotency key %s): %s", a.req.IdempotencyKey, describe(err))
		return result{code: ExitUnavailable}
	}
	a.r.logf("submission refused: %s", describe(err))
	return result{code: ExitSoftware}
}

func (a *attempt) open(ctx context.Context) (<-chan streamMsg, context.CancelFunc) {
	sctx, cancel := context.WithCancel(ctx)
	var stream grpc.ServerStreamingClient[pb.JobEvent]
	var err error
	if a.jobID == "" {
		// A no-wait submission is retransmitted with the same key; a
		// duplicate returns the committed job or the committed busy result.
		stream, err = a.r.client.TrySubmitAndWatch(sctx, a.req)
	} else {
		stream, err = a.r.client.WatchJob(sctx, &pb.WatchJobRequest{JobId: a.jobID, Runner: a.r.inc})
	}
	ch := make(chan streamMsg, 16)
	go func() {
		if err != nil {
			select {
			case ch <- streamMsg{err: err}:
			case <-sctx.Done():
			}
			return
		}
		for {
			ev, err := stream.Recv()
			select {
			case ch <- streamMsg{ev: ev, err: err}:
			case <-sctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return ch, cancel
}

func (a *attempt) loop(ctx context.Context) result {
	var (
		msgs         <-chan streamMsg
		stop         = context.CancelFunc(func() {})
		reconnect    <-chan time.Time
		lastMsg      = time.Now()
		failingSince time.Time
		backoff      = 50 * time.Millisecond
		interrupt    = a.r.interrupt.Done()
	)
	defer func() {
		stop()
		a.cancelClaim()
		a.waitReports(ctx)
	}()
	connect := func() {
		stop()
		msgs, stop = a.open(ctx)
		lastMsg = time.Now()
	}
	retryLater := func() {
		stop()
		msgs = nil
		if a.noWatch {
			return
		}
		if failingSince.IsZero() {
			failingSince = time.Now()
		}
		reconnect = time.After(backoff)
		backoff = min(backoff*2, maxBackoff)
	}
	connect()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case m := <-msgs:
			if m.err != nil {
				if res, done := a.streamFailed(m.err); done {
					return res
				}
				retryLater()
				continue
			}
			lastMsg, failingSince, backoff = time.Now(), time.Time{}, 50*time.Millisecond
			if res, done := a.handle(ctx, m.ev); done {
				return res
			}
		case <-reconnect:
			reconnect = nil
			connect()
		case exit := <-a.exited:
			if res, done := a.childExited(exit); done {
				return res
			}
		case report := <-a.reports:
			a.finishReport(report)
			if reportsCancellation(report) {
				if res, done := a.cancel(ctx); done {
					return res
				}
			}
			if report.name == "finish" {
				if res, done := a.finishReported(report.err); done {
					return res
				}
				interrupt = a.r.interrupt.Done()
			}
		case claim := <-a.claims:
			if res, done := a.claimCompleted(ctx, claim); done {
				return res
			}
		case <-interrupt:
			if a.finishPending {
				interrupt = nil
				continue
			}
			a.cancelClaim()
			return a.abandon(ctx)
		case <-tick.C:
			if msgs != nil && time.Since(lastMsg) > 3*a.heartbeat {
				a.r.logf("no heartbeat from the broker; reconnecting")
				retryLater()
			}
			if !a.awaitFinal.IsZero() && time.Since(a.awaitFinal) > a.r.o.ReconnectWindow {
				a.r.logf("release of job %s was not confirmed; not retrying", a.jobID)
				return result{code: a.code, jobID: a.jobID}
			}
			if !a.running && !failingSince.IsZero() && time.Since(failingSince) > a.r.o.ReconnectWindow {
				return a.unreachable()
			}
		}
	}
}

func (a *attempt) unreachable() result {
	switch {
	case a.ran:
		a.r.logf("broker unreachable; job %s's reservation stays held until the broker verifies the command exited", a.jobID)
		return result{code: a.code, jobID: a.jobID}
	case a.jobID == "":
		a.r.logf("broker unreachable; submission outcome unknown (idempotency key %s)", a.req.IdempotencyKey)
	default:
		a.r.logf("broker unreachable; job %s was not launched and is left to the broker", a.jobID)
	}
	return result{code: ExitUnavailable, jobID: a.jobID}
}

// streamFailed decides whether a stream error ends the attempt. Transient
// failures and expired event buffers reconnect with the same incarnation.
func (a *attempt) streamFailed(err error) (result, bool) {
	if errors.Is(err, io.EOF) {
		return result{}, false
	}
	switch reason(err) {
	case pb.ErrorReason_EVENTS_EXPIRED:
		return result{}, false
	case pb.ErrorReason_ADMISSION_BUSY:
		a.r.logf("broker is busy; not admitted (--no-wait)")
		return result{code: ExitBusy}, true
	}
	if transient(err) {
		return result{}, false
	}
	if a.jobID == "" {
		return a.submitFailed(err), true
	}
	if a.running {
		a.r.logf("stopped watching job %s: %s", a.jobID, describe(err))
		a.noWatch = true
		return result{}, false
	}
	a.r.logf("job %s: %s", a.jobID, describe(err))
	if a.ran {
		return result{code: a.code, jobID: a.jobID}, true
	}
	return result{code: ExitSoftware, jobID: a.jobID}, true
}

func (a *attempt) handle(ctx context.Context, ev *pb.JobEvent) (result, bool) {
	switch e := ev.GetEvent().(type) {
	case *pb.JobEvent_Snapshot:
		if a.jobID == "" {
			a.jobID = e.Snapshot.GetJobId()
			a.r.logf("submitted job %s", a.jobID)
		}
		return a.apply(ctx, e.Snapshot)
	case *pb.JobEvent_Granted:
		if a.grant == nil {
			a.grant = e.Granted
		}
		return a.launch()
	case *pb.JobEvent_CancelRequested:
		return a.cancel(ctx)
	case *pb.JobEvent_Finished:
		return a.finished(e.Finished)
	case *pb.JobEvent_QueuePositionChanged:
		a.waiting(e.QueuePositionChanged.GetPosition(), e.QueuePositionChanged.GetQueueLength())
	case *pb.JobEvent_Heartbeat:
		if interval := e.Heartbeat.GetHeartbeatInterval(); interval != nil {
			heartbeat := interval.AsDuration()
			if err := interval.CheckValid(); err != nil || heartbeat <= 0 || heartbeat > time.Duration(math.MaxInt64/3) {
				a.r.logf("broker sent an invalid heartbeat interval")
			} else {
				a.heartbeat = heartbeat
			}
		}
	}
	return result{}, false
}

func (a *attempt) waiting(position, length uint32) {
	if a.position != nil && *a.position == position {
		return
	}
	a.position = &position
	if length > 0 {
		a.r.logf("waiting for admission: position %d of %d", position+1, length)
	} else {
		a.r.logf("waiting for admission: position %d", position+1)
	}
}

// apply treats a snapshot as level-triggered state: a grant calls for a
// durable claim, a cancellation request for cancellation.
func (a *attempt) apply(ctx context.Context, job *pb.Job) (result, bool) {
	if job.GetState() == pb.JobState_JOB_STATE_FINISHED {
		return a.finished(job)
	}
	if a.grant == nil && job.Grant != nil {
		a.grant = job.Grant
	}
	if job.CancelRequestedAt != nil {
		if res, done := a.cancel(ctx); done {
			return res, true
		}
	}
	if job.QueuePosition != nil && a.grant == nil {
		a.waiting(job.GetQueuePosition(), 0)
	}
	if a.grant != nil {
		return a.launch()
	}
	return result{}, false
}

func (a *attempt) launch() (result, bool) {
	if a.fenced || a.cancelSeen || a.grant == nil || a.claims != nil {
		return result{}, false
	}
	// Only the loop can act on a claim; it must stay responsive to
	// cancellation even while the claim's outcome is ambiguous.
	claimCtx, stop := context.WithCancel(a.r.interrupt)
	a.stopClaim = stop
	a.claims = make(chan claimResult, 1)
	claims, r, jobID, grant := a.claims, a.r, a.jobID, a.grant.GetGrantId()
	go func() {
		resp, err := retry(claimCtx, r.o.ReconnectWindow, func(c context.Context) (*pb.ClaimLaunchResponse, error) {
			return r.client.ClaimLaunch(c, &pb.ClaimLaunchRequest{JobId: jobID, GrantId: grant, Runner: r.inc})
		})
		claims <- claimResult{response: resp, err: err}
	}()
	return result{}, false
}

func (a *attempt) cancelClaim() {
	if a.stopClaim != nil {
		a.stopClaim()
		a.stopClaim = nil
	}
	a.claims = nil
}

func (a *attempt) claimCompleted(ctx context.Context, claim claimResult) (result, bool) {
	a.cancelClaim()
	if a.fenced || a.cancelSeen {
		return result{}, false
	}
	resp, err := claim.response, claim.err
	if err != nil {
		if _, ok := a.r.interrupted(); ok {
			return a.abandon(ctx), true
		}
		a.fenced = true
		if transient(err) {
			a.r.logf("launch claim for job %s is unconfirmed; not launching. Its reservation stays held until the broker resolves it: %s", a.jobID, describe(err))
			return result{code: ExitUnavailable, jobID: a.jobID}, true
		}
		a.r.logf("launch claim for job %s refused: %s", a.jobID, describe(err))
		if reason(err) == pb.ErrorReason_GRANT_FENCED {
			return result{code: ExitCancelled, jobID: a.jobID}, true
		}
		return result{code: ExitSoftware, jobID: a.jobID}, true
	}
	if resp.GetGrant().GetGrantId() != a.grant.GetGrantId() {
		a.fenced = true
		a.r.logf("broker returned a different grant for job %s; not launching", a.jobID)
		return result{code: ExitSoftware, jobID: a.jobID}, true
	}
	a.claimed = true
	return a.spawn(ctx)
}

// spawn starts the command exactly once. The local fence is set first and
// the start is serialized with the signal router, so a runner interrupt
// either prevents the spawn or is forwarded to the started command.
func (a *attempt) spawn(ctx context.Context) (result, bool) {
	a.fenced = true
	cmd := newCommand(a.r.o, withMarker(a.r.o.Env, a.jobID))
	a.r.mu.Lock()
	if a.r.signal != 0 {
		a.r.mu.Unlock()
		return a.abandon(ctx), true
	}
	err := cmd.Err
	start := time.Now()
	if err == nil {
		err = cmd.Start()
	}
	if err == nil {
		a.r.child = cmd.Process
		a.r.childSignalable = true
	}
	a.r.mu.Unlock()
	if err != nil {
		a.r.logf("launch failed: %v", err)
		a.reportNoLaunch(ctx, a.r.o.ReconnectWindow, false, err.Error())
		return result{code: launchExitCode(err), jobID: a.jobID}, true
	}
	a.running, a.cmd = true, cmd
	// Observe the root before it can be reaped, so its identity is exact.
	proc, perr := procid.Of(cmd.Process.Pid)
	go func() {
		a.exited <- waitForCommand(cmd, start, func() { a.r.disarmChild(cmd.Process) }, a.r.logf)
	}()
	jobID, grant, r := a.jobID, a.grant.GetGrantId(), a.r
	a.report("seal", func() (*pb.Job, error) {
		return retry(ctx, r.o.ReconnectWindow, func(c context.Context) (*pb.Job, error) {
			return r.client.SealLaunch(c, &pb.SealLaunchRequest{JobId: jobID, GrantId: grant, Runner: r.inc})
		})
	})
	if perr != nil {
		a.r.logf("could not observe command identity (pid %d): %v; job %s's reservation stays held", cmd.Process.Pid, perr, a.jobID)
		return result{}, false
	}
	a.proc = proc
	a.report("start", func() (*pb.Job, error) {
		return retry(ctx, r.o.ReconnectWindow, func(c context.Context) (*pb.Job, error) {
			return r.client.ReportStarted(c, &pb.ReportStartedRequest{JobId: jobID, GrantId: grant, Process: proc, Runner: r.inc})
		})
	})
	return result{}, false
}

func (a *attempt) report(name string, send func() (*pb.Job, error)) {
	if a.reports == nil {
		a.reports = make(chan reportResult, 3)
	}
	a.pending++
	reports := a.reports
	go func() {
		job, err := send()
		reports <- reportResult{name: name, job: job, err: err}
	}()
}

func (a *attempt) finishReport(report reportResult) {
	a.pending--
	if report.err != nil {
		if report.name == "finish" {
			a.r.logf("finish report for job %s not delivered: %s; its reservation stays held until the broker verifies the command exited", a.jobID, describe(report.err))
		} else {
			a.r.logf("%s report for job %s not delivered: %s", report.name, a.jobID, describe(report.err))
		}
	}
}

func reportsCancellation(report reportResult) bool {
	return report.err == nil && report.job != nil && report.job.CancelRequestedAt != nil
}

func (a *attempt) waitReports(ctx context.Context) {
	for a.pending > 0 {
		report := <-a.reports
		a.finishReport(report)
		if a.ran && a.claimed && !a.cancelSeen && reportsCancellation(report) {
			a.cancelSeen = true
			a.acknowledgeCancellation(ctx)
		}
	}
}

// reportNoLaunch fences the grant with no spawn and reports why.
func (a *attempt) reportNoLaunch(ctx context.Context, window time.Duration, cancelled bool, detail string) {
	grant := a.grant.GetGrantId()
	if _, err := retry(ctx, window, func(c context.Context) (*pb.Job, error) {
		return a.r.client.SealLaunch(c, &pb.SealLaunchRequest{JobId: a.jobID, GrantId: grant, Runner: a.r.inc, NoCommandSpawned: true})
	}); err != nil {
		a.r.logf("no-launch seal for job %s not delivered: %s", a.jobID, describe(err))
	}
	if _, err := retry(ctx, window, func(c context.Context) (*pb.Job, error) {
		return a.r.client.ReportLaunchFailed(c, &pb.ReportLaunchFailedRequest{
			JobId: a.jobID, GrantId: grant, Error: detail, Cancelled: cancelled, Runner: a.r.inc,
		})
	}); err != nil {
		a.r.logf("launch failure report for job %s not delivered: %s", a.jobID, describe(err))
	}
}

func (a *attempt) acknowledgeCancellation(ctx context.Context) {
	jobID, grant, r := a.jobID, a.grant.GetGrantId(), a.r
	a.report("cancellation acknowledgment", func() (*pb.Job, error) {
		return retry(ctx, r.o.ReconnectWindow, func(c context.Context) (*pb.Job, error) {
			return r.client.AcknowledgeCancel(c, &pb.AcknowledgeCancelRequest{JobId: jobID, GrantId: grant, Runner: r.inc})
		})
	})
}

func (a *attempt) cancel(ctx context.Context) (result, bool) {
	if a.cancelSeen {
		return result{}, false
	}
	a.cancelSeen = true
	switch {
	case a.running:
		a.r.logf("job %s cancelled; stopping the command", a.jobID)
		jobID, grant, r := a.jobID, a.grant.GetGrantId(), a.r
		// Only the unreaped root can be targeted safely by this runner.
		if err := r.killChild(a.cmd.Process); err != nil && !errors.Is(err, os.ErrProcessDone) {
			a.report("cancellation failure", func() (*pb.Job, error) {
				return retry(ctx, r.o.ReconnectWindow, func(c context.Context) (*pb.Job, error) {
					return r.client.ReportCancelFailed(c, &pb.ReportCancelFailedRequest{JobId: jobID, GrantId: grant, Runner: r.inc, Error: err.Error()})
				})
			})
		} else {
			a.acknowledgeCancellation(ctx)
		}
	case a.claims != nil || a.claimed && !a.fenced:
		a.fenced = true
		a.cancelClaim()
		a.acknowledgeCancellation(ctx)
		a.reportNoLaunch(ctx, a.r.o.ReconnectWindow, true, "cancelled before launch")
		return result{code: ExitCancelled, jobID: a.jobID}, true
	case a.claimed && a.ran:
		a.acknowledgeCancellation(ctx)
	default:
		// Unclaimed: the broker fences the grant itself and finishes the job.
		a.fenced = true
	}
	return result{}, false
}

func (a *attempt) childExited(exit childExit) (result, bool) {
	a.running, a.ran = false, true
	a.r.setChild(nil)
	if exit.state == nil {
		a.r.logf("could not obtain the command's exit status: %v; job %s's reservation stays held", exit.err, a.jobID)
		return result{code: ExitSoftware, jobID: a.jobID}, true
	}
	var exitError *exec.ExitError
	if exit.err != nil && !errors.As(exit.err, &exitError) {
		a.r.logf("waiting for the command reported an I/O error: %v", exit.err)
	}
	if exit.timingErr != nil {
		a.r.logf("could not observe root runtime for job %s: %v", a.jobID, exit.timingErr)
	}
	code, outcome := exitStatus(exit.state)
	a.code = code
	if a.proc == nil {
		return result{code: code, jobID: a.jobID}, true
	}
	a.finishPending = true
	a.retryEligible = a.r.o.Retries > 0 && code != 0 && outcome.GetKind() == pb.OutcomeKind_OUTCOME_KIND_EXITED &&
		!a.grant.GetForced()
	jobID, grant, proc, r := a.jobID, a.grant.GetGrantId(), a.proc, a.r
	a.report("finish", func() (*pb.Job, error) {
		// Once the outcome is known, a runner signal must not drop it.
		return retry(context.WithoutCancel(r.interrupt), r.o.ReconnectWindow, func(c context.Context) (*pb.Job, error) {
			return r.client.ReportFinished(c, &pb.ReportFinishedRequest{
				JobId: jobID, GrantId: grant, Outcome: outcome, Runner: r.inc, Process: proc,
				Elapsed: optionalMilliseconds(exit.elapsedMs), RootCpuTime: optionalMilliseconds(exit.rootCPUMs),
			})
		})
	})
	return result{}, false
}

func (a *attempt) finishReported(err error) (result, bool) {
	a.finishPending = false
	if err != nil || !a.retryEligible || a.cancelSeen || a.noWatch || a.r.signalled() {
		return result{code: a.code, jobID: a.jobID}, true
	}
	// A retry needs the broker's verified release, signalled by finished.
	a.awaitFinal = time.Now()
	// A finished event may precede the response to the report that caused it.
	if a.finishedWhileReporting != nil {
		return a.finished(a.finishedWhileReporting)
	}
	return result{}, false
}

func optionalMilliseconds(ms *int64) *durationpb.Duration {
	if ms == nil {
		return nil
	}
	return contract.MillisecondDuration(*ms)
}

func (a *attempt) finished(job *pb.Job) (result, bool) {
	if a.running {
		// The command is still this runner's; keep waiting for it.
		a.noWatch = true
		return result{}, false
	}
	o := job.GetOutcome()
	if a.ran {
		if a.finishPending {
			a.finishedWhileReporting = job
			return result{}, false
		}
		eligible := !a.awaitFinal.IsZero() && o.GetKind() == pb.OutcomeKind_OUTCOME_KIND_EXITED &&
			o.GetExitCode() != 0 && job.CancelRequestedAt == nil && !job.GetForced()
		return result{code: a.code, jobID: a.jobID, retryable: eligible}, true
	}
	switch o.GetKind() {
	case pb.OutcomeKind_OUTCOME_KIND_CANCELLED_BEFORE_START:
		a.r.logf("job %s was cancelled before it started", a.jobID)
		return result{code: ExitCancelled, jobID: a.jobID}, true
	case pb.OutcomeKind_OUTCOME_KIND_REMOVED_DISCONNECTED:
		a.r.logf("job %s was removed after its runner disconnected", a.jobID)
	case pb.OutcomeKind_OUTCOME_KIND_EXITED:
		return result{code: int(o.GetExitCode()), jobID: a.jobID}, true
	case pb.OutcomeKind_OUTCOME_KIND_SIGNALED:
		return result{code: 128 + int(o.GetSignalNumber()), jobID: a.jobID}, true
	default:
		a.r.logf("job %s finished without launching here: %s", a.jobID, o.GetKind())
	}
	return result{code: ExitSoftware, jobID: a.jobID}, true
}

// abandon handles a runner signal while no command runs. An existing grant
// is claimed and fenced with no spawn so its capacity can be released;
// a queued job is left to the broker's disconnect grace.
func (a *attempt) abandon(ctx context.Context) result {
	sig, _ := a.r.interrupted()
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if a.grant != nil && !a.ran && (!a.fenced || a.claimed) && !a.running {
		a.fenced = true
		if !a.claimed {
			if _, err := a.r.client.ClaimLaunch(c, &pb.ClaimLaunchRequest{JobId: a.jobID, GrantId: a.grant.GetGrantId(), Runner: a.r.inc}); err == nil {
				a.claimed = true
			}
		}
		if a.claimed {
			a.reportNoLaunch(c, time.Second, false, "runner interrupted before launch")
		}
	}
	if a.jobID != "" {
		a.r.logf("interrupted; job %s was not launched", a.jobID)
	}
	return result{code: 128 + int(sig), jobID: a.jobID}
}

func newCommand(o Options, env []string) *exec.Cmd {
	cmd := exec.Command(o.Command[0], o.Command[1:]...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = o.Stdin, o.Stdout, o.Stderr
	if o.Stdin == nil {
		cmd.Stdin = os.Stdin
	}
	if o.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if o.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	return cmd
}

func launchExitCode(err error) int {
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return ExitNotFound
	}
	return ExitCannotExecute
}

// direct runs a nested command inside an already admitted job.
func direct(o Options, logf func(string, ...any)) int {
	cmd := newCommand(o, o.Env)
	err := cmd.Err
	start := time.Now()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		logf("launch failed: %v", err)
		return launchExitCode(err)
	}
	owner := &runner{child: cmd.Process, childSignalable: true, logf: logf}
	done := make(chan struct{})
	defer close(done)
	if o.Signals != nil {
		go func() {
			for {
				select {
				case <-done:
					return
				case s := <-o.Signals:
					if sig, ok := s.(syscall.Signal); ok && sig != syscall.SIGINT {
						if err := owner.signalChild(cmd.Process, sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
							logf("could not forward signal to the command: %v", err)
						}
					}
				}
			}
		}()
	}
	exit := waitForCommand(cmd, start, func() { owner.disarmChild(cmd.Process) }, logf)
	if exit.state == nil {
		logf("could not obtain the command's exit status: %v", exit.err)
		return ExitSoftware
	}
	code, _ := exitStatus(exit.state)
	return code
}
