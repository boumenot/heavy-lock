// Package runner is the v2 job runner: it submits a command to the per-user
// broker, waits for its grant, durably claims it, spawns the command without
// a shell, reports its lifecycle and propagates its exit status.
package runner

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/boumenot/heavy-lock/apps/tuna/attribution"
	"github.com/boumenot/heavy-lock/apps/tuna/contract"
	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"github.com/boumenot/heavy-lock/apps/tuna/procid"
	"golang.org/x/sys/unix"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const (
	ExitUsage       = 64
	ExitUnavailable = 69
	ExitSoftware    = 70
	// ExitBusy is used only for a committed no-wait ADMISSION_BUSY refusal.
	ExitBusy          = 75
	ExitCannotExecute = 126
	ExitNotFound      = 127
	// ExitCancelled reports a job cancelled before its command started.
	ExitCancelled = 130

	// MaxRetries is the fixed upper bound of opt-in retries.
	MaxRetries = 3

	// MarkerVariable is the v2 nesting marker: the job ID under which the
	// enclosing command was admitted. A nested runner that sees it runs its
	// command directly. The v1 HEAVY_LOCK_HELD marker is never treated as
	// v2 admission.
	MarkerVariable = "TUNA_JOB_ID"

	HeavyWeight = 2
	LightWeight = 1

	// DefaultReconnectWindow bounds how long the runner keeps retrying an
	// unreachable broker while no command is running. A running command is
	// never abandoned for broker loss.
	DefaultReconnectWindow = 2 * time.Minute
	healthTimeout          = 5 * time.Second
	defaultHeartbeat       = 5 * time.Second
	maxBackoff             = 2 * time.Second
)

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// ParseWeight accepts heavy, light or a positive integer.
func ParseWeight(value string) (uint32, bool) {
	switch value {
	case "heavy":
		return HeavyWeight, true
	case "light":
		return LightWeight, true
	}
	n, err := strconv.ParseUint(value, 10, 32)
	if err != nil || n < 1 {
		return 0, false
	}
	return uint32(n), true
}

type Options struct {
	Port   int
	Weight uint32
	// Attribution holds the explicit --repository and --worklog options;
	// the rest of a job's attribution is detected at submission.
	Attribution attribution.Options
	NoWait      bool
	Retries     int
	Command     []string
	Version     string

	// Child standard streams; nil inherits the runner's.
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// Log receives runner diagnostics; nil discards them.
	Log io.Writer
	// Env is the runner environment; nil uses os.Environ().
	Env []string
	// Signals delivers SIGINT, SIGTERM and SIGHUP to the runner.
	Signals <-chan os.Signal

	ReconnectWindow time.Duration
	// Git reads the job checkout's attribution; nil runs git with Env.
	Git attribution.Git
}

// Run executes one invocation and returns the process exit code.
func Run(ctx context.Context, o Options) int {
	if o.Env == nil {
		o.Env = os.Environ()
	}
	if o.ReconnectWindow <= 0 {
		o.ReconnectWindow = DefaultReconnectWindow
	}
	if o.Weight == 0 {
		o.Weight = HeavyWeight
	}
	logf := func(format string, args ...any) {
		if o.Log != nil {
			_, _ = fmt.Fprintf(o.Log, "tuna: "+format+"\n", args...)
		}
	}
	if len(o.Command) == 0 {
		logf("a command is required after --")
		return ExitUsage
	}
	if o.Retries < 0 || o.Retries > MaxRetries {
		logf("--retries must be between 0 and %d", MaxRetries)
		return ExitUsage
	}
	if marker, ok := lookup(o.Env, MarkerVariable); ok {
		if !uuidV4.MatchString(marker) {
			logf("%s is not a v2 job ID; refusing to run unadmitted", MarkerVariable)
			return ExitUsage
		}
		logf("already admitted as job %s; running directly", marker)
		return direct(o, logf)
	}
	command, err := contract.RenderCommand(o.Command)
	if err != nil {
		logf("%v", err)
		return ExitUsage
	}
	if runtime.GOOS != "darwin" {
		logf("the v2 runner is implemented only on macOS")
		return ExitSoftware
	}

	conn, err := grpc.NewClient(fmt.Sprintf("127.0.0.1:%d", o.Port),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		logf("connect to broker: %v", err)
		return ExitUnavailable
	}
	defer conn.Close()

	// A new submission fails immediately when the broker is unavailable:
	// no v1 fallback, no broker start-up and no waiting for a restart.
	hctx, cancel := context.WithTimeout(ctx, healthTimeout)
	health, err := pb.NewControlServiceClient(conn).GetHealth(hctx, &pb.GetHealthRequest{})
	cancel()
	if err == nil {
		// Submissions include ClientInfo.os_version, introduced in API 2.2.
		err = contract.CheckService(health, o.Version, 2)
	}
	if err == nil && !health.Ready {
		err = errors.New("broker is recovering and not accepting submissions")
	}
	if err != nil {
		logf("v2 broker unavailable on 127.0.0.1:%d: %s", o.Port, describe(err))
		return ExitUnavailable
	}

	self, err := procid.Self()
	if err != nil {
		logf("observe runner process identity: %v", err)
		return ExitSoftware
	}
	r := &runner{
		o:       o,
		client:  pb.NewRunnerServiceClient(conn),
		inc:     &pb.RunnerIncarnation{IncarnationId: newUUID(), Process: self},
		logf:    logf,
		command: command,
		info:    clientInfo(o.Version),
	}
	r.cwd, r.attribution = r.detectAttribution(ctx)
	r.interrupt, r.stopInterrupt = context.WithCancel(ctx)
	defer r.stopInterrupt()
	r.routeSignals()

	result := r.attempt(ctx, "", o.NoWait)
	for i := 0; i < o.Retries && result.retryable && !r.signalled(); i++ {
		logf("job %s exited %d; retry %d of %d", result.jobID, result.code, i+1, o.Retries)
		result = r.attempt(ctx, result.jobID, true)
	}
	return result.code
}

type runner struct {
	o       Options
	client  pb.RunnerServiceClient
	inc     *pb.RunnerIncarnation
	logf    func(string, ...any)
	command string
	info    *pb.ClientInfo // the reported client, shared by every attempt
	// cwd and attribution are observed once and shared by every attempt.
	cwd         string
	attribution *pb.Attribution

	// interrupt is cancelled when a runner signal arrives while no command
	// runs; signal records that signal.
	interrupt     context.Context
	stopInterrupt context.CancelFunc

	mu              sync.Mutex
	child           *os.Process // the command, until its wait completes
	childSignalable bool
	signal          syscall.Signal
	// anySignal records any runner signal, including one forwarded to a
	// running command; a deliberate interruption is never retried.
	anySignal bool
}

func (r *runner) routeSignals() {
	if r.o.Signals == nil {
		return
	}
	go func() {
		for {
			select {
			case <-r.interrupt.Done():
				return
			case s := <-r.o.Signals:
				sig, ok := s.(syscall.Signal)
				if !ok {
					continue
				}
				r.mu.Lock()
				r.anySignal = true
				child := r.child
				var signalErr error
				if child == nil {
					r.signal = sig
				} else if r.childSignalable && sig != syscall.SIGINT {
					// Keep the fence held through signaling so the PID
					// cannot be reaped and reused in between.
					signalErr = child.Signal(sig)
				}
				r.mu.Unlock()
				if child == nil {
					r.stopInterrupt()
					return
				}
				if signalErr != nil && !errors.Is(signalErr, os.ErrProcessDone) {
					r.logf("could not forward signal to the command: %v", signalErr)
				}
			}
		}
	}()
}

// setChild records the running command; signals arriving before it is set
// interrupt the runner instead.
func (r *runner) setChild(p *os.Process) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.child = p
	r.childSignalable = p != nil
}

func (r *runner) disarmChild(p *os.Process) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.child == p {
		r.childSignalable = false
	}
}

func (r *runner) killChild(p *os.Process) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.child != p || !r.childSignalable {
		return os.ErrProcessDone
	}
	return p.Kill()
}

func (r *runner) signalled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.anySignal
}

func (r *runner) interrupted() (syscall.Signal, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.signal, r.signal != 0
}

type result struct {
	code      int
	jobID     string
	retryable bool
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func lookup(env []string, name string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		if value, ok := strings.CutPrefix(env[i], name+"="); ok {
			return value, true
		}
	}
	return "", false
}

func withMarker(env []string, jobID string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, MarkerVariable+"=") {
			out = append(out, kv)
		}
	}
	return append(out, MarkerVariable+"="+jobID)
}

// reason returns the broker's structured error reason, or UNSPECIFIED for
// transport failures that carry none.
func reason(err error) pb.ErrorReason {
	for _, d := range status.Convert(err).Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok && info.Domain == contract.ServiceIdentity {
			return pb.ErrorReason(pb.ErrorReason_value[info.Reason])
		}
	}
	return pb.ErrorReason_ERROR_REASON_UNSPECIFIED
}

// transient errors leave a request's outcome unknown, or come from a broker
// that cannot accept it yet. The same request is retried, never replaced.
func transient(err error) bool {
	switch reason(err) {
	case pb.ErrorReason_ERROR_REASON_UNSPECIFIED:
		switch status.Code(err) {
		case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled, codes.Unknown, codes.Internal:
			return true
		}
	case pb.ErrorReason_RECOVERING, pb.ErrorReason_STORAGE_UNAVAILABLE,
		pb.ErrorReason_DEADLINE_EXCEEDED, pb.ErrorReason_REQUEST_CANCELLED:
		return true
	}
	return false
}

func describe(err error) string {
	if s, ok := status.FromError(err); ok {
		if r := reason(err); r != pb.ErrorReason_ERROR_REASON_UNSPECIFIED {
			return fmt.Sprintf("%s (%s)", s.Message(), r)
		}
		return s.Message()
	}
	return err.Error()
}

// retry repeats call with backoff while its errors are transient, until ctx
// ends or window elapses; a zero window retries until ctx ends.
func retry[T any](ctx context.Context, window time.Duration, call func(context.Context) (T, error)) (T, error) {
	var deadline time.Time
	if window > 0 {
		deadline = time.Now().Add(window)
	}
	backoff := 50 * time.Millisecond
	for {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		v, err := call(cctx)
		cancel()
		if err == nil || !transient(err) {
			return v, err
		}
		if ctx.Err() != nil {
			return v, errors.Join(err, ctx.Err())
		}
		if !deadline.IsZero() && time.Now().Add(backoff).After(deadline) {
			return v, err
		}
		select {
		case <-ctx.Done():
			return v, errors.Join(err, ctx.Err())
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (r *runner) submitRequest(retryOf string) *pb.SubmitJobRequest {
	req := &pb.SubmitJobRequest{
		IdempotencyKey: newUUID(),
		Command:        r.command,
		Weight:         r.o.Weight,
		Client:         r.info,
		Runner:         r.inc,
		Attribution:    r.attribution,
	}
	if r.cwd != "" {
		req.Cwd = ptr(r.cwd)
	}
	if retryOf != "" {
		req.RetryOfJobId = &retryOf
	}
	return req
}

func clientInfo(version string) *pb.ClientInfo {
	c := &pb.ClientInfo{
		Name:         ptr("tuna"),
		Os:           attribution.ClientOS(runtime.GOOS),
		Architecture: architecture(),
	}
	if version != "" {
		c.Version = ptr(version)
	}
	if v := attribution.OSVersion(); v != "" {
		c.OsVersion = ptr(v)
	}
	return c
}

// detectAttribution combines the explicit options with the working
// directory's git checkout and the invoking agent session.
func (r *runner) detectAttribution(ctx context.Context) (string, *pb.Attribution) {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}
	var checkout attribution.Checkout
	if cwd != "" {
		git := r.o.Git
		if git == nil {
			git = attribution.RunGit(r.o.Env)
		}
		checkout = attribution.DetectCheckout(ctx, cwd, git)
	}
	getenv := func(name string) string {
		value, _ := lookup(r.o.Env, name)
		return value
	}
	return cwd, attribution.Build(r.o.Attribution, checkout, getenv)
}

func architecture() pb.ClientArchitecture {
	switch runtime.GOARCH {
	case "arm64":
		return pb.ClientArchitecture_CLIENT_ARCHITECTURE_ARM64
	case "amd64":
		return pb.ClientArchitecture_CLIENT_ARCHITECTURE_AMD64
	}
	return pb.ClientArchitecture_CLIENT_ARCHITECTURE_OTHER
}

func ptr[T any](v T) *T { return &v }

// exitStatus maps a reaped child's status to the runner's exit code and the
// reported outcome.
func exitStatus(state *os.ProcessState) (int, *pb.Outcome) {
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		sig := ws.Signal()
		return 128 + int(sig), &pb.Outcome{
			Kind:         pb.OutcomeKind_OUTCOME_KIND_SIGNALED,
			SignalNumber: ptr(int32(sig)),
			SignalName:   ptr(signalName(sig)),
		}
	}
	code := state.ExitCode()
	return code, &pb.Outcome{Kind: pb.OutcomeKind_OUTCOME_KIND_EXITED, ExitCode: ptr(int64(code))}
}

func signalName(sig syscall.Signal) string {
	if name := unix.SignalName(sig); name != "" {
		return name
	}
	return "SIG" + strconv.Itoa(int(sig))
}
