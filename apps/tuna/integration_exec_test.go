package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	pb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var submittedJob = regexp.MustCompile(`submitted job ([0-9a-f-]{36})`)

func TestServiceAdmissionSettings(t *testing.T) {
	t.Setenv("TUNA_TEST_LOCK_DIR", filepath.Join(t.TempDir(), "singleton"))
	port := freePort(t)
	dir := filepath.Join(t.TempDir(), "state")
	for _, tt := range []struct {
		name     string
		args     []string
		capacity uint32
		backfill uint32
	}{
		{name: "explicit strict FIFO", args: []string{"--capacity", "4", "--backfill-limit", "0"}, capacity: 4, backfill: 0},
		{name: "restarted with different settings", args: []string{"--capacity", "2", "--backfill-limit", "3"}, capacity: 2, backfill: 3},
		{name: "default backfill", args: []string{"--capacity", "3"}, capacity: 3, backfill: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			broker := startService(t, port, dir, tt.args...)
			code, _, body := restCall(t, port, http.MethodGet, "/status", nil)
			if code != http.StatusOK {
				t.Fatalf("status = %d: %s", code, body)
			}
			var status struct {
				Capacity      uint32 `json:"capacity"`
				BackfillLimit uint32 `json:"backfillLimit"`
			}
			if err := json.Unmarshal([]byte(body), &status); err != nil {
				t.Fatal(err)
			}
			if status.Capacity != tt.capacity || status.BackfillLimit != tt.backfill {
				t.Fatalf("status = %+v, want capacity %d and backfill %d", status, tt.capacity, tt.backfill)
			}
			broker.stop(t)
		})
	}
}

type runnerProcess struct {
	fencedTestProcess
	stderr *syncBuffer
	done   chan error
	exited bool
}

func startRunner(t *testing.T, args ...string) *runnerProcess {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	p := &runnerProcess{stderr: &syncBuffer{}, done: make(chan error, 1)}
	p.cmd = tunaCommand(ctx, args...)
	p.cmd.Cancel = func() error { return p.signal(syscall.SIGTERM) }
	p.cmd.Stderr = p.stderr
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p.signalable = true
	go func() { p.done <- p.wait() }()
	t.Cleanup(func() { p.stop(t) })
	return p
}

func (p *runnerProcess) stop(t *testing.T) {
	t.Helper()
	if p.exited {
		return
	}
	if err := p.signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("stop runner: %v", err)
	}
	select {
	case <-p.done:
		p.exited = true
	case <-time.After(10 * time.Second):
		t.Errorf("runner did not stop; stderr=%s", p.stderr.String())
	}
}

func (p *runnerProcess) jobID(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if m := submittedJob.FindStringSubmatch(p.stderr.String()); m != nil {
			return m[1]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("runner did not submit; stderr=%s", p.stderr.String())
	return ""
}

func (p *runnerProcess) exitCode(t *testing.T) int {
	t.Helper()
	select {
	case err := <-p.done:
		p.exited = true
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		if err != nil {
			t.Fatal(err)
		}
		return 0
	case <-time.After(30 * time.Second):
		t.Fatalf("runner did not exit; stderr=%s", p.stderr.String())
		return -1
	}
}

func waitJobState(t *testing.T, port, id, state string) {
	t.Helper()
	want := regexp.MustCompile(`"state":\s*"` + state + `"`)
	deadline := time.Now().Add(20 * time.Second)
	var out, errOut string
	for time.Now().Before(deadline) {
		var code int
		code, out, errOut = tuna(t, "job", "--port", port, "--job", id, "--json")
		if code == 0 && want.MatchString(out) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("job %s never reached %s: %q %q", id, state, out, errOut)
}

// TestExecutableRunnerWithControls drives #13's runner with #14's controls
// through one real broker process.
func TestExecutableRunnerWithControls(t *testing.T) {
	t.Setenv("TUNA_TEST_LOCK_DIR", filepath.Join(t.TempDir(), "singleton"))
	port := freePort(t)
	startService(t, port, filepath.Join(t.TempDir(), "state"))
	ps := fmt.Sprint(port)

	if code, out, errOut := tuna(t, "pause", "--port", ps); code != 0 {
		t.Fatalf("pause = %d %q %q", code, out, errOut)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	if code, _, errOut := tuna(t, "--port", ps, "--no-wait", "--", "/usr/bin/touch", marker); code != 75 {
		t.Fatalf("no-wait while paused = %d %q; want 75", code, errOut)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused command ran: %v", err)
	}
	if code, out, errOut := tuna(t, "resume", "--port", ps); code != 0 {
		t.Fatalf("resume = %d %q %q", code, out, errOut)
	}

	a := startRunner(t, "--port", ps, "--worklog", "13", "--", "/bin/sleep", "60")
	aID := a.jobID(t)
	waitJobState(t, ps, aID, "JOB_STATE_RUNNING")

	b := startRunner(t, "--port", ps, "--retries", "2", "--", "/bin/sh", "-c", "exit 3")
	bID := b.jobID(t)
	waitJobState(t, ps, bID, "JOB_STATE_QUEUED")
	if code, out, errOut := tuna(t, "queue", "--port", ps, "--json"); code != 0 || !strings.Contains(out, bID) {
		t.Fatalf("queue = %d %q %q; want %s", code, out, errOut, bID)
	}
	if code, out, errOut := tuna(t, "force-start", "--port", ps, "--job", bID); code != 0 {
		t.Fatalf("force-start = %d %q %q", code, out, errOut)
	}
	if code := b.exitCode(t); code != 3 {
		t.Fatalf("force-started runner = %d; stderr=%s", code, b.stderr.String())
	}
	if n := len(submittedJob.FindAllString(b.stderr.String(), -1)); n != 1 {
		t.Fatalf("force-started job was retried: %d submissions; stderr=%s", n, b.stderr.String())
	}

	if status, _, body := restCall(t, port, http.MethodPost, "/jobs/"+aID+"/cancel", nil); status != 200 {
		t.Fatalf("REST cancel = %d %s", status, body)
	}
	if code := a.exitCode(t); code != 137 {
		t.Fatalf("cancelled runner = %d; stderr=%s", code, a.stderr.String())
	}
	waitJobState(t, ps, aID, "JOB_STATE_FINISHED")
	waitJobState(t, ps, bID, "JOB_STATE_FINISHED")
	if status, _, body := restCall(t, port, http.MethodGet, "/jobs/"+aID, nil); status != 200 || !strings.Contains(body, "JOB_STATE_FINISHED") {
		t.Fatalf("REST job = %d %s", status, body)
	}
}

func TestBrokerRestartPreservesLiveRunnerAndReservation(t *testing.T) {
	t.Setenv("TUNA_TEST_LOCK_DIR", filepath.Join(t.TempDir(), "singleton"))
	port, dir := freePort(t), filepath.Join(t.TempDir(), "state")
	service := startService(t, port, dir)
	ps := fmt.Sprint(port)
	marker := filepath.Join(t.TempDir(), "launches")
	r := startRunner(t, "--port", ps, "--", "/bin/sh", "-c",
		`printf x >> "$0"; exec /bin/sleep 60`, marker)
	id := r.jobID(t)
	waitJobState(t, ps, id, "JOB_STATE_RUNNING")

	readJob := func() *pb.Job {
		t.Helper()
		status, _, body := restCall(t, port, http.MethodGet, "/jobs/"+id, nil)
		if status != 200 {
			t.Fatalf("read job = %d %s", status, body)
		}
		var job pb.Job
		if err := protojson.Unmarshal([]byte(body), &job); err != nil {
			t.Fatal(err)
		}
		return &job
	}
	before := readJob()
	if before.Process == nil || before.Grant == nil {
		t.Fatalf("live job has no process or grant: %v", before)
	}
	service.stop(t)
	startService(t, port, dir)
	t.Cleanup(func() { r.stop(t) })
	after := readJob()
	if !proto.Equal(after.Process, before.Process) || !proto.Equal(after.Grant.Runner, before.Grant.Runner) {
		t.Fatalf("restart changed live identities: before=%v after=%v", before, after)
	}
	status, _, body := restCall(t, port, http.MethodGet, "/status", nil)
	var brokerStatus pb.BrokerStatus
	if status != 200 {
		t.Fatalf("status after restart = %d %s", status, body)
	}
	if err := protojson.Unmarshal([]byte(body), &brokerStatus); err != nil {
		t.Fatal(err)
	}
	if brokerStatus.Occupied != 2 || brokerStatus.UnreconciledReservations != 1 {
		t.Fatalf("recovered accounting = %v", &brokerStatus)
	}
	if status, _, body = restCall(t, port, http.MethodPost, "/jobs/"+id+"/cancel", nil); status != 200 {
		t.Fatalf("cancel after restart = %d %s", status, body)
	}
	if code := r.exitCode(t); code != 137 {
		t.Fatalf("cancelled runner = %d; stderr=%s", code, r.stderr.String())
	}
	waitJobState(t, ps, id, "JOB_STATE_FINISHED")
	if contents, err := os.ReadFile(marker); err != nil || string(contents) != "x" {
		t.Fatalf("restart launched command more than once: %q, %v", contents, err)
	}
}
