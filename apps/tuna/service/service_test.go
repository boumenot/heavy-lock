package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/boumenot/heavy-lock/apps/tuna/broker"
	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"github.com/boumenot/heavy-lock/apps/tuna/procid"
	"github.com/boumenot/heavy-lock/apps/tuna/runner"
	"github.com/boumenot/heavy-lock/apps/tuna/scheduler"
	"github.com/boumenot/heavy-lock/apps/tuna/storage"
	"github.com/boumenot/heavy-lock/apps/tuna/transport"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type idleSampler struct{}

func (idleSampler) Sample(context.Context) (scheduler.Sample, error) {
	return scheduler.Sample{TotalMemoryBytes: 1000, AvailableMemoryBytes: 900, LoadAverage1m: 0.1, CPULoadPercent: 1}, nil
}

// syncBuffer is a log sink that tests can wait on.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

var jobLine = regexp.MustCompile(`submitted job ([0-9a-f-]{36})`)

func (b *syncBuffer) jobID(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if m := jobLine.FindStringSubmatch(b.String()); m != nil {
			return m[1]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no job submitted; log:\n%s", b.String())
	return ""
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// testBroker is the production composition over a temporary database, with
// access to the scheduler so tests can drive control actions.
type testBroker struct {
	port   int
	sched  *scheduler.Scheduler
	server *transport.Server
	stop   func()
}

func startBroker(t *testing.T, capacity uint32) *testBroker {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "broker.db"))
	if err != nil {
		t.Fatal(err)
	}
	hub := broker.NewHub()
	cfg := scheduler.DefaultConfig(capacity)
	cfg.Settle = 0
	sched, _, err := scheduler.Start(ctx, store, scheduler.Options{
		Config: cfg, ServiceVersion: "test", PID: int64(os.Getpid()), Sampler: idleSampler{},
		Publish: hub.Publish, Processes: procid.Probe{},
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := newBroker(sched, store, hub, Options{ServiceVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	server, err := transport.Listen(freePort(t), b)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve() }()
	if _, err := sched.MarkReady(ctx); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); maintain(ctx, sched, b, t.Logf) }()
	tb := &testBroker{port: server.Port(), sched: sched, server: server}
	var once sync.Once
	tb.stop = func() {
		once.Do(func() {
			_ = server.Close()
			cancel()
			wg.Wait()
			_ = sched.Stop(context.Background())
			_ = store.Close()
		})
	}
	t.Cleanup(tb.stop)
	return tb
}

func (b *testBroker) waitState(t *testing.T, id string, want pb.JobState) *pb.Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		j, err := b.sched.Job(context.Background(), id)
		if err == nil && j.State == want {
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s: state %v err %v, want %v", id, j.GetState(), err, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// cleanEnv is the test environment without a nesting marker.
func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, runner.MarkerVariable+"=") {
			env = append(env, kv)
		}
	}
	return env
}

type invocation struct {
	o      runner.Options
	log    *syncBuffer
	stdout *syncBuffer
	sigs   chan os.Signal
	done   chan int
}

func (b *testBroker) start(t *testing.T, adjust func(*runner.Options), command ...string) *invocation {
	t.Helper()
	inv := &invocation{log: &syncBuffer{}, stdout: &syncBuffer{}, sigs: make(chan os.Signal, 4), done: make(chan int, 1)}
	inv.o = runner.Options{Port: b.port, Command: command, Version: "test", Env: cleanEnv(),
		Stdin: strings.NewReader(""), Stdout: inv.stdout, Stderr: inv.log, Log: inv.log, Signals: inv.sigs,
		ReconnectWindow: 5 * time.Second}
	if adjust != nil {
		adjust(&inv.o)
	}
	go func() { inv.done <- runner.Run(context.Background(), inv.o) }()
	return inv
}

func (inv *invocation) wait(t *testing.T) int {
	t.Helper()
	select {
	case code := <-inv.done:
		return code
	case <-time.After(20 * time.Second):
		t.Fatalf("runner did not exit; log:\n%s", inv.log.String())
		return -1
	}
}

func (b *testBroker) run(t *testing.T, adjust func(*runner.Options), command ...string) (int, *invocation) {
	t.Helper()
	inv := b.start(t, adjust, command...)
	return inv.wait(t), inv
}

func TestRunnerPropagatesOutcomesAndReleasesCapacity(t *testing.T) {
	b := startBroker(t, 2)
	notExec := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(notExec, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		command []string
		code    int
		outcome pb.OutcomeKind
	}{
		{"success", []string{"/usr/bin/true"}, 0, pb.OutcomeKind_OUTCOME_KIND_EXITED},
		{"exit code", []string{"/bin/sh", "-c", "exit 7"}, 7, pb.OutcomeKind_OUTCOME_KIND_EXITED},
		{"signal", []string{"/bin/sh", "-c", "kill -TERM $$"}, 143, pb.OutcomeKind_OUTCOME_KIND_SIGNALED},
		{"not found", []string{filepath.Join(t.TempDir(), "missing")}, 127, pb.OutcomeKind_OUTCOME_KIND_LAUNCH_FAILED},
		{"not executable", []string{notExec}, 126, pb.OutcomeKind_OUTCOME_KIND_LAUNCH_FAILED},
	}
	// Each heavy job needs the whole capacity, so every run proves the
	// previous reservation was released.
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, inv := b.run(t, nil, tt.command...)
			if code != tt.code {
				t.Fatalf("exit %d, want %d; log:\n%s", code, tt.code, inv.log.String())
			}
			j := b.waitState(t, inv.log.jobID(t), pb.JobState_JOB_STATE_FINISHED)
			if j.Outcome.GetKind() != tt.outcome {
				t.Fatalf("outcome %v, want %v", j.Outcome, tt.outcome)
			}
		})
	}
}

func TestRunnerSetsMarkerAndRunsWithoutShell(t *testing.T) {
	b := startBroker(t, 2)
	code, inv := b.run(t, nil, "/usr/bin/printenv", runner.MarkerVariable)
	if code != 0 {
		t.Fatalf("exit %d; log:\n%s", code, inv.log.String())
	}
	if got := strings.TrimSpace(inv.stdout.String()); got != inv.log.jobID(t) {
		t.Fatalf("marker %q, want the job ID", got)
	}
	// Arguments are passed verbatim, with no shell expansion.
	code, inv = b.run(t, nil, "/bin/echo", "$HOME", "a b", "*")
	if code != 0 || inv.stdout.String() != "$HOME a b *\n" {
		t.Fatalf("exit %d, output %q", code, inv.stdout.String())
	}
}

func TestNestedRunBypassesBroker(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	env := append(cleanEnv(), runner.MarkerVariable+"=1b4e28ba-2fa1-41d2-883f-0016d3cca427")
	var log bytes.Buffer
	// The port has no broker: a nested run must not need one.
	code := runner.Run(context.Background(), runner.Options{Port: freePort(t), Env: env, Log: &log,
		Command: []string{"/bin/sh", "-c", `touch "$0"; exit 4`, marker}})
	if code != 4 {
		t.Fatalf("exit %d; log: %s", code, log.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("nested command did not run")
	}

	env = append(cleanEnv(), runner.MarkerVariable+"=1")
	if code := runner.Run(context.Background(), runner.Options{Port: 1, Env: env, Log: &log, Command: []string{"/usr/bin/true"}}); code != runner.ExitUsage {
		t.Fatalf("invalid marker exit %d, want %d", code, runner.ExitUsage)
	}
}

func TestUnreachableBrokerFailsFast(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	var log bytes.Buffer
	start := time.Now()
	code := runner.Run(context.Background(), runner.Options{Port: freePort(t), Env: cleanEnv(), Log: &log,
		Command: []string{"/usr/bin/touch", marker}})
	if code != runner.ExitUnavailable {
		t.Fatalf("exit %d, want %d; log: %s", code, runner.ExitUnavailable, log.String())
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("unreachable broker did not fail fast")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("command ran without a broker")
	}
}

func counter(t *testing.T) (string, func() int) {
	file := filepath.Join(t.TempDir(), "count")
	return file, func() int {
		data, err := os.ReadFile(file)
		if err != nil {
			return 0
		}
		return strings.Count(string(data), "x")
	}
}

func TestRetries(t *testing.T) {
	b := startBroker(t, 2)
	retries := func(n int) func(*runner.Options) { return func(o *runner.Options) { o.Retries = n } }

	file, count := counter(t)
	flaky := []string{"/bin/sh", "-c", `printf x >> "$0"; [ "$(wc -c < "$0")" -ge 2 ]`, file}
	if code, inv := b.run(t, retries(2), flaky...); code != 0 || count() != 2 {
		t.Fatalf("flaky: exit %d after %d runs; log:\n%s", code, count(), inv.log.String())
	}

	file, count = counter(t)
	if code, _ := b.run(t, nil, "/bin/sh", "-c", `printf x >> "$0"; exit 3`, file); code != 3 || count() != 1 {
		t.Fatalf("default: exit %d after %d runs; retries must be opt-in", code, count())
	}

	file, count = counter(t)
	if code, _ := b.run(t, retries(runner.MaxRetries), "/bin/sh", "-c", `printf x >> "$0"; exit 3`, file); code != 3 || count() != 1+runner.MaxRetries {
		t.Fatalf("limit: exit %d after %d runs", code, count())
	}

	// Signals and launch failures are not eligible.
	file, count = counter(t)
	if code, _ := b.run(t, retries(3), "/bin/sh", "-c", `printf x >> "$0"; kill -TERM $$`, file); code != 143 || count() != 1 {
		t.Fatalf("signal: exit %d after %d runs", code, count())
	}
	if code, _ := b.run(t, retries(3), filepath.Join(t.TempDir(), "missing")); code != 127 {
		t.Fatalf("launch failure exit %d", code)
	}

	// A runner signal forwarded to a command that turns it into a normal
	// nonzero exit is still a deliberate interruption, not a retry.
	file, count = counter(t)
	trapped := b.start(t, retries(3), "/bin/sh", "-c",
		`trap 'exit 1' TERM; printf x >> "$0"; while :; do sleep 0.1; done`, file)
	for deadline := time.Now().Add(10 * time.Second); count() == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("trapping command did not start; log:\n%s", trapped.log.String())
		}
	}
	trapped.sigs <- syscall.SIGTERM
	if code := trapped.wait(t); code != 1 || count() != 1 {
		t.Fatalf("signalled: exit %d after %d runs; log:\n%s", code, count(), trapped.log.String())
	}
}

func TestNoWaitBusyDoesNotRun(t *testing.T) {
	b := startBroker(t, 2)
	holder := b.start(t, nil, "/bin/sleep", "30")
	b.waitState(t, holder.log.jobID(t), pb.JobState_JOB_STATE_RUNNING)

	marker := filepath.Join(t.TempDir(), "ran")
	code, inv := b.run(t, func(o *runner.Options) { o.NoWait = true }, "/usr/bin/touch", marker)
	if code != runner.ExitBusy {
		t.Fatalf("exit %d, want %d; log:\n%s", code, runner.ExitBusy, inv.log.String())
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("busy no-wait command ran")
	}
	holder.sigs <- syscall.SIGTERM
	if code := holder.wait(t); code != 143 {
		t.Fatalf("forwarded SIGTERM exit %d, want 143", code)
	}
}

func TestCancelRunningJobForcesCommand(t *testing.T) {
	b := startBroker(t, 2)
	inv := b.start(t, nil, "/bin/sleep", "30")
	id := inv.log.jobID(t)
	b.waitState(t, id, pb.JobState_JOB_STATE_RUNNING)
	if _, err := b.sched.Cancel(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	if code := inv.wait(t); code != 128+int(syscall.SIGKILL) {
		t.Fatalf("exit %d; log:\n%s", code, inv.log.String())
	}
	j := b.waitState(t, id, pb.JobState_JOB_STATE_FINISHED)
	if j.Outcome.GetKind() != pb.OutcomeKind_OUTCOME_KIND_SIGNALED || j.CancelRequestedAtUnixNs == nil {
		t.Fatalf("job %+v", j)
	}
}

func TestQueuedRunnerInterruptAndCancel(t *testing.T) {
	b := startBroker(t, 2)
	holder := b.start(t, nil, "/bin/sleep", "30")
	b.waitState(t, holder.log.jobID(t), pb.JobState_JOB_STATE_RUNNING)

	marker := filepath.Join(t.TempDir(), "ran")
	interrupted := b.start(t, nil, "/usr/bin/touch", marker)
	b.waitState(t, interrupted.log.jobID(t), pb.JobState_JOB_STATE_QUEUED)
	interrupted.sigs <- syscall.SIGINT
	if code := interrupted.wait(t); code != 130 {
		t.Fatalf("interrupted queued runner exit %d; log:\n%s", code, interrupted.log.String())
	}

	cancelled := b.start(t, nil, "/usr/bin/touch", marker)
	id := cancelled.log.jobID(t)
	b.waitState(t, id, pb.JobState_JOB_STATE_QUEUED)
	if _, err := b.sched.Cancel(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	if code := cancelled.wait(t); code != runner.ExitCancelled {
		t.Fatalf("cancelled queued runner exit %d; log:\n%s", code, cancelled.log.String())
	}
	holder.sigs <- syscall.SIGTERM
	holder.wait(t)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an interrupted or cancelled queued command ran")
	}
}

func TestBrokerLossKeepsCommandAndReservation(t *testing.T) {
	b := startBroker(t, 2)
	inv := b.start(t, func(o *runner.Options) { o.ReconnectWindow = 500 * time.Millisecond },
		"/bin/sh", "-c", "sleep 1; exit 5")
	id := inv.log.jobID(t)
	b.waitState(t, id, pb.JobState_JOB_STATE_RUNNING)
	if err := b.server.Close(); err != nil {
		t.Fatal(err)
	}
	// The command is not killed by broker loss and its status propagates;
	// the unconfirmed release stays held rather than being guessed.
	if code := inv.wait(t); code != 5 {
		t.Fatalf("exit %d; log:\n%s", code, inv.log.String())
	}
	if !strings.Contains(inv.log.String(), "stays held") {
		t.Fatalf("log does not report the held reservation:\n%s", inv.log.String())
	}
	if j, err := b.sched.Job(context.Background(), id); err != nil || j.State == pb.JobState_JOB_STATE_FINISHED {
		t.Fatalf("job %v err %v; release must wait for the finish report", j.GetState(), err)
	}
}

func TestSingletonLock(t *testing.T) {
	dir := t.TempDir()
	lock, err := AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireLock(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock err = %v, want ErrLocked", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	again, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	_ = again.Release()
}

func TestRunServesRunnersAndExcludesSecondBroker(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan int, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Port: freePort(t), DataDir: dir, Capacity: 2, ServiceVersion: "test",
			Sampler: idleSampler{}, Ready: func(port int) { ready <- port }})
	}()
	var port int
	select {
	case port = <-ready:
	case err := <-done:
		t.Fatal(err)
	}
	var log bytes.Buffer
	code := runner.Run(context.Background(), runner.Options{Port: port, Env: cleanEnv(), Log: &log,
		Command: []string{"/bin/sh", "-c", "exit 3"}, Stdin: strings.NewReader("")})
	if code != 3 {
		t.Fatalf("exit %d; log: %s", code, log.String())
	}
	if err := Run(context.Background(), Options{Port: freePort(t), DataDir: dir, Capacity: 2, Sampler: idleSampler{}}); !errors.Is(err, ErrLocked) {
		t.Fatalf("second broker err = %v, want ErrLocked", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReplacementRunnerCannotClaimOrReport(t *testing.T) {
	b := startBroker(t, 2)
	holder := b.start(t, nil, "/bin/sleep", "30")
	id := holder.log.jobID(t)
	j := b.waitState(t, id, pb.JobState_JOB_STATE_RUNNING)

	conn, err := grpc.NewClient(fmt.Sprintf("127.0.0.1:%d", b.port), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := pb.NewRunnerServiceClient(conn)
	self, err := procid.Self()
	if err != nil {
		t.Fatal(err)
	}
	impostor := &pb.RunnerIncarnation{IncarnationId: "1b4e28ba-2fa1-41d2-883f-0016d3cca427", Process: self}
	ctx := context.Background()
	if _, err := client.ClaimLaunch(ctx, &pb.ClaimLaunchRequest{JobId: id, GrantId: j.Grant.GrantId, Runner: impostor}); contractReason(err) != pb.ErrorReason_CLAIM_MISMATCH {
		t.Fatalf("replacement claim err = %v", err)
	}
	if _, err := client.ReportFinished(ctx, &pb.ReportFinishedRequest{JobId: id, GrantId: j.Grant.GrantId, Runner: impostor,
		Process: j.Process, Outcome: &pb.Outcome{Kind: pb.OutcomeKind_OUTCOME_KIND_EXITED, ExitCode: proto.Int64(0)}}); contractReason(err) != pb.ErrorReason_CLAIM_MISMATCH {
		t.Fatalf("replacement finish err = %v", err)
	}
	if got := b.waitState(t, id, pb.JobState_JOB_STATE_RUNNING); got.Outcome != nil {
		t.Fatal("a replacement runner changed the job")
	}
	holder.sigs <- syscall.SIGTERM
	holder.wait(t)
}

func contractReason(err error) pb.ErrorReason {
	for _, d := range status.Convert(err).Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return pb.ErrorReason(pb.ErrorReason_value[info.Reason])
		}
	}
	return pb.ErrorReason_ERROR_REASON_UNSPECIFIED
}
