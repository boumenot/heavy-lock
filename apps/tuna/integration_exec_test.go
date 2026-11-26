package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var submittedJob = regexp.MustCompile(`submitted job ([0-9a-f-]{36})`)

type runnerProcess struct {
	cmd    *exec.Cmd
	stderr *syncBuffer
	done   chan error
}

func startRunner(t *testing.T, args ...string) *runnerProcess {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	p := &runnerProcess{stderr: &syncBuffer{}, done: make(chan error, 1)}
	p.cmd = tunaCommand(ctx, args...)
	var env []string
	for _, kv := range p.cmd.Env {
		if !strings.HasPrefix(kv, "TUNA_JOB_ID=") {
			env = append(env, kv)
		}
	}
	p.cmd.Env, p.cmd.Stderr = env, p.stderr
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.done <- p.cmd.Wait() }()
	return p
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
	port := freePort(t)
	startService(t, port, t.TempDir())
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

	if status, _, body := restCall(t, port, http.MethodPost, "/v1/jobs/"+aID+"/cancel", nil); status != 200 {
		t.Fatalf("REST cancel = %d %s", status, body)
	}
	if code := a.exitCode(t); code != 137 {
		t.Fatalf("cancelled runner = %d; stderr=%s", code, a.stderr.String())
	}
	waitJobState(t, ps, aID, "JOB_STATE_FINISHED")
	waitJobState(t, ps, bID, "JOB_STATE_FINISHED")
	if status, _, body := restCall(t, port, http.MethodGet, "/v1/jobs/"+aID, nil); status != 200 || !strings.Contains(body, "JOB_STATE_FINISHED") {
		t.Fatalf("REST job = %d %s", status, body)
	}
}
