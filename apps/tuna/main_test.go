package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/boumenot/heavy-lock/apps/tuna/cli"
	"github.com/boumenot/heavy-lock/apps/tuna/runner"
	"github.com/boumenot/heavy-lock/apps/tuna/service"
)

type recorded struct {
	service *service.Options
	runner  *runner.Options
}

func (r *recorded) launcher() launcher {
	return launcher{
		service: func(o service.Options) int { r.service = &o; return 0 },
		runner:  func(o runner.Options) int { r.runner = &o; return 0 },
		control: func(args []string, stdout, stderr io.Writer) (int, bool) {
			return cli.Run(context.Background(), args, stdout, stderr)
		},
	}
}

func TestDispatch(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		code       int
		wantOutput string
		wantError  string
	}{
		{name: "help", args: []string{"--help"}, code: 0, wantOutput: "tuna <control>"},
		{name: "short help", args: []string{"-h"}, code: 0, wantOutput: "Usage: tuna"},
		{name: "missing command", code: 64, wantError: "Usage: tuna"},
		{name: "empty command", args: []string{"--port", "1", "--"}, code: 64, wantError: "Usage: tuna"},
		{name: "unknown runner option", args: []string{"--port", "1", "--bogus", "--", "echo"}, code: 64, wantError: `unknown runner option "--bogus"`},
		{name: "unknown service option", args: []string{"service", "--weight", "1"}, code: 64, wantError: "-weight"},
		{name: "service extra", args: []string{"service", "x"}, code: 64, wantError: "invalid service arguments"},
		{name: "service help", args: []string{"service", "--help"}, code: 0, wantOutput: "tuna service"},
		{name: "control help", args: []string{"status", "--help"}, code: 0, wantOutput: "force-start"},
		{name: "control usage", args: []string{"cancel"}, code: 64, wantError: "--job is required"},
		{name: "bad service port", args: []string{"service", "--port", "0"}, code: 64, wantError: "invalid service arguments"},
		{name: "bad weight", args: []string{"--port", "1", "--weight", "0", "--", "echo"}, code: 64, wantError: "--weight"},
		{name: "bad worklog", args: []string{"--port", "1", "--worklog", "x", "--", "echo"}, code: 64, wantError: "--worklog"},
		{name: "too many retries", args: []string{"--port", "1", "--retries", "4", "--", "echo"}, code: 64, wantError: "--retries"},
		{name: "negative retries", args: []string{"--port", "1", "--retries=-1", "--", "echo"}, code: 64, wantError: "--retries"},
		{name: "duplicate weight", args: []string{"--port", "1", "--weight", "1", "--weight=2", "--", "echo"}, code: 64, wantError: "once"},
		{name: "duplicate no-wait", args: []string{"--port", "1", "--no-wait", "--no-wait", "--", "echo"}, code: 64, wantError: "once"},
		{name: "missing value", args: []string{"--port", "1", "--weight", "--", "echo"}, code: 64, wantError: "--weight"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			var r recorded
			if got := dispatch(tt.args, &stdout, &stderr, r.launcher()); got != tt.code {
				t.Fatalf("exit code = %d, want %d; stderr = %q", got, tt.code, stderr.String())
			}
			if !strings.Contains(stdout.String(), tt.wantOutput) {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantOutput)
			}
			if !strings.Contains(stderr.String(), tt.wantError) {
				t.Errorf("stderr = %q, want %q", stderr.String(), tt.wantError)
			}
			if tt.code != 0 && (r.runner != nil || r.service != nil) {
				t.Fatal("an invalid invocation must not launch")
			}
		})
	}
}

func TestDispatchRunnerOptions(t *testing.T) {
	var r recorded
	var stderr bytes.Buffer
	args := []string{"--port=49790", "--weight", "light", "--worklog=13", "--no-wait", "--retries", "3", "--", "make", "--", "-j"}
	if got := dispatch(args, &bytes.Buffer{}, &stderr, r.launcher()); got != 0 {
		t.Fatalf("exit code = %d; stderr = %q", got, stderr.String())
	}
	o := r.runner
	if o == nil || o.Port != 49790 || o.Weight != runner.LightWeight || o.Worklog != "13" || !o.NoWait || o.Retries != 3 {
		t.Fatalf("options = %+v", o)
	}
	if !reflect.DeepEqual(o.Command, []string{"make", "--", "-j"}) {
		t.Fatalf("command = %q; arguments after the first -- belong to the command", o.Command)
	}

	r = recorded{}
	if got := dispatch([]string{"--port", "1", "--", "echo"}, &bytes.Buffer{}, &stderr, r.launcher()); got != 0 {
		t.Fatalf("exit code = %d", got)
	}
	if r.runner.Weight != runner.HeavyWeight || r.runner.Retries != 0 || r.runner.NoWait {
		t.Fatalf("defaults = %+v; want heavy, no retries, queued", r.runner)
	}
}

func TestDispatchService(t *testing.T) {
	var r recorded
	if got := dispatch([]string{"service"}, &bytes.Buffer{}, &bytes.Buffer{}, r.launcher()); got != 0 || r.service.Port != 49783 {
		t.Fatalf("code %d, options %+v; want default port 49783", got, r.service)
	}
	r = recorded{}
	args := []string{"service", "--port=50000", "--data-dir", "/tmp/x", "--event-max-age", "1h", "--event-max-count", "7"}
	if got := dispatch(args, &bytes.Buffer{}, &bytes.Buffer{}, r.launcher()); got != 0 || r.service.Port != 50000 ||
		r.service.DataDir != "/tmp/x" || r.service.EventMaxAge != time.Hour || r.service.EventMaxCount != 7 || r.service.ServiceVersion != version {
		t.Fatalf("code %d, options %+v", got, r.service)
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func TestHelpReportsWriteFailure(t *testing.T) {
	var stderr bytes.Buffer
	if got := run([]string{"--help"}, brokenWriter{}, &stderr); got != 70 {
		t.Fatalf("exit code = %d, want 70", got)
	}
}

func TestRunnerRequiresExplicitPort(t *testing.T) {
	tests := []struct {
		name string
		args []string
		code int
	}{
		{name: "missing", args: []string{"--", "echo"}, code: 64},
		{name: "not a number", args: []string{"--port", "abc", "--", "echo"}, code: 64},
		{name: "zero", args: []string{"--port", "0", "--", "echo"}, code: 64},
		{name: "too large", args: []string{"--port", "65536", "--", "echo"}, code: 64},
		{name: "duplicate", args: []string{"--port", "49783", "--port", "49784", "--", "echo"}, code: 64},
		{name: "valid", args: []string{"--port", "49783", "--", "echo"}, code: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			var r recorded
			if got := dispatch(tt.args, &stdout, &stderr, r.launcher()); got != tt.code {
				t.Fatalf("exit code = %d, want %d; stderr = %q", got, tt.code, stderr.String())
			}
		})
	}
}
