package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const execEnv = "TUNA_TEST_EXEC_MAIN"

// TestMain lets the test binary act as the real tuna executable, so the
// public entry point is exercised in separate processes.
func TestMain(m *testing.M) {
	if os.Getenv(execEnv) == "1" {
		os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

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

func tunaCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	cmd.Env = append(os.Environ(), execEnv+"=1")
	return cmd
}

func tuna(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := tunaCommand(ctx, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, stdout.String(), stderr.String()
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

type serviceProcess struct {
	cmd    *exec.Cmd
	stderr *syncBuffer
	done   chan error
	exited bool
}

func startService(t *testing.T, port int, dir string) *serviceProcess {
	t.Helper()
	p := &serviceProcess{stderr: &syncBuffer{}, done: make(chan error, 1)}
	p.cmd = tunaCommand(context.Background(), "service", "--port", fmt.Sprint(port), "--data-dir", dir)
	p.cmd.Stdout, p.cmd.Stderr = io.Discard, p.stderr
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.done <- p.cmd.Wait() }()
	t.Cleanup(func() {
		if !p.exited {
			_ = p.cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-p.done:
			case <-time.After(10 * time.Second):
				t.Errorf("service did not stop; stderr=%s", p.stderr.String())
			}
		}
	})
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(p.stderr.String(), "ready on 127.0.0.1:") {
		select {
		case err := <-p.done:
			p.exited = true
			t.Fatalf("service exited early: %v; stderr=%s", err, p.stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("service not ready; stderr=%s", p.stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	return p
}

func (p *serviceProcess) stop(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-p.done:
		p.exited = true
		if err != nil {
			t.Fatalf("service exit: %v; stderr=%s", err, p.stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("service did not stop; stderr=%s", p.stderr.String())
	}
}

func restCall(t *testing.T, port int, method, path string, header map[string]string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, string(body)
}

func TestExecutableServiceAndControls(t *testing.T) {
	dir := t.TempDir()
	port := freePort(t)
	p := startService(t, port, dir)
	ps := fmt.Sprint(port)

	code, out, errOut := tuna(t, "status", "--port", ps, "--json")
	if code != 0 || !strings.Contains(out, `"capacity"`) || !strings.Contains(out, `"paused": false`) && !strings.Contains(out, `"paused":false`) {
		t.Fatalf("status = %d %q %q", code, out, errOut)
	}
	if code, out, errOut = tuna(t, "pause", "--port", ps); code != 0 || !strings.Contains(out, "paused: true") {
		t.Fatalf("pause = %d %q %q", code, out, errOut)
	}
	if status, _, body := restCall(t, port, http.MethodGet, "/v1/status", nil); status != 200 || !strings.Contains(body, `"paused":true`) {
		t.Fatalf("REST after CLI pause = %d %s", status, body)
	}
	if status, _, body := restCall(t, port, http.MethodPost, "/v1/queue/resume", nil); status != 200 || strings.Contains(body, `"paused":true`) {
		t.Fatalf("REST resume = %d %s", status, body)
	}
	if code, out, errOut = tuna(t, "status", "--port", ps); code != 0 || !strings.Contains(out, "paused: false") {
		t.Fatalf("CLI after REST resume = %d %q %q", code, out, errOut)
	}
	for _, args := range [][]string{{"queue"}, {"jobs"}} {
		if code, out, errOut = tuna(t, append(args, "--port", ps)...); code != 0 {
			t.Fatalf("%v = %d %q %q", args, code, out, errOut)
		}
	}

	missing := "00000000-0000-4000-8000-000000000000"
	for _, tc := range []struct {
		cli  []string
		rest string
	}{
		{[]string{"job", "--job", missing}, "GET /v1/jobs/" + missing},
		{[]string{"cancel", "--job", missing}, "POST /v1/jobs/" + missing + "/cancel"},
		{[]string{"force-start", "--job", missing}, "POST /v1/jobs/" + missing + "/force-start"},
	} {
		code, out, errOut := tuna(t, append(tc.cli, "--port", ps)...)
		if code != 70 || out != "" || !strings.Contains(errOut, "NotFound/JOB_NOT_FOUND") {
			t.Fatalf("%v = %d %q %q", tc.cli, code, out, errOut)
		}
		method, path, _ := strings.Cut(tc.rest, " ")
		if status, _, body := restCall(t, port, method, path, nil); status != 404 || !strings.Contains(body, "JOB_NOT_FOUND") {
			t.Fatalf("%s = %d %s", tc.rest, status, body)
		}
	}
	if code, _, errOut = tuna(t, "job", "--job", "not-a-uuid", "--port", ps); code != 70 || !strings.Contains(errOut, "InvalidArgument/INVALID_ARGUMENT_FIELD") {
		t.Fatalf("invalid job = %d %q", code, errOut)
	}

	status, header, _ := restCall(t, port, http.MethodOptions, "/v1/queue/pause", map[string]string{
		"Origin": "https://example.invalid", "Access-Control-Request-Method": "POST"})
	if status != 204 || header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("preflight = %d %v", status, header)
	}

	if code, _, errOut = tuna(t, "service", "--port", ps, "--data-dir", t.TempDir()); code != 70 || !strings.Contains(errOut, "already in use") {
		t.Fatalf("port conflict = %d %q", code, errOut)
	}
	if code, _, errOut = tuna(t, "service", "--port", fmt.Sprint(freePort(t)), "--data-dir", dir); code != 75 || !strings.Contains(errOut, "singleton lock") {
		t.Fatalf("lock conflict = %d %q", code, errOut)
	}

	if code, out, errOut = tuna(t, "pause", "--port", ps); code != 0 {
		t.Fatalf("pause = %d %q %q", code, out, errOut)
	}
	p.stop(t)
	if code, _, errOut = tuna(t, "status", "--port", ps, "--timeout", "1s"); code != 70 || !strings.Contains(errOut, "broker unavailable") {
		t.Fatalf("stopped broker = %d %q", code, errOut)
	}

	startService(t, port, dir)
	if code, out, errOut = tuna(t, "status", "--port", ps); code != 0 || !strings.Contains(out, "paused: true") {
		t.Fatalf("restarted status = %d %q %q", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "broker.db")); err != nil {
		t.Fatal(err)
	}
}
