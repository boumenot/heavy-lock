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

	"github.com/boumenot/tuna/apps/tuna/cli"
	"github.com/boumenot/tuna/apps/tuna/runner"
	"github.com/boumenot/tuna/apps/tuna/service"
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
			return cli.Run(context.Background(), args, stdout, stderr, version)
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
		{name: "bare worklog repository", args: []string{"--port", "1", "--worklog", "tuna#3", "--", "echo"}, code: 64, wantError: "--worklog"},
		{name: "bad repository", args: []string{"--port", "1", "--repository", "tuna", "--", "echo"}, code: 64, wantError: "--repository must be"},
		{name: "duplicate repository", args: []string{"--port", "1", "--repository", "a/b", "--repository=a/b", "--", "echo"}, code: 64, wantError: "once"},
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
	args := []string{"--port=49790", "--weight", "light", "--worklog=13", "--repository", "github.com/boumenot/tuna", "--no-wait", "--retries", "3", "--", "make", "--", "-j"}
	if got := dispatch(args, &bytes.Buffer{}, &stderr, r.launcher()); got != 0 {
		t.Fatalf("exit code = %d; stderr = %q", got, stderr.String())
	}
	o := r.runner
	if o == nil || o.Port != 49790 || o.Weight != runner.LightWeight || o.Attribution.Worklog == nil || o.Attribution.Worklog.String() != "13" ||
		o.Attribution.Repository == nil || o.Attribution.Repository.String() != "github.com/boumenot/tuna" || !o.NoWait || o.Retries != 3 {
		t.Fatalf("options = %+v", o)
	}
	if !reflect.DeepEqual(o.Command, []string{"make", "--", "-j"}) {
		t.Fatalf("command = %q; arguments after the first -- belong to the command", o.Command)
	}

	r = recorded{}
	if got := dispatch([]string{"--port", "1", "--worklog", "ghe.example/org/plans#4", "--", "echo"}, &bytes.Buffer{}, &stderr, r.launcher()); got != 0 {
		t.Fatalf("exit code = %d; stderr = %q", got, stderr.String())
	}
	if w := r.runner.Attribution.Worklog; w == nil || w.URL() != "https://ghe.example/org/plans/issues/4" || r.runner.Attribution.Repository != nil {
		t.Fatalf("qualified worklog options = %+v", r.runner.Attribution)
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

func TestDispatchServiceAdmissionSettings(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		capacity uint32
		backfill uint32
	}{
		{name: "configured", args: []string{"--capacity", "4", "--backfill-limit", "0"}, capacity: 4, backfill: 0},
		{name: "max values", args: []string{"--capacity=4294967295", "--backfill-limit=4294967295"}, capacity: 4294967295, backfill: 4294967295},
		{name: "zero capacity", args: []string{"--capacity", "0"}, wantCode: usageExitCode},
		{name: "negative capacity", args: []string{"--capacity=-1"}, wantCode: usageExitCode},
		{name: "capacity overflow", args: []string{"--capacity=4294967296"}, wantCode: usageExitCode},
		{name: "empty capacity", args: []string{"--capacity="}, wantCode: usageExitCode},
		{name: "negative backfill", args: []string{"--backfill-limit=-1"}, wantCode: usageExitCode},
		{name: "backfill overflow", args: []string{"--backfill-limit=4294967296"}, wantCode: usageExitCode},
		{name: "empty backfill", args: []string{"--backfill-limit="}, wantCode: usageExitCode},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r recorded
			var stderr bytes.Buffer
			code := dispatch(append([]string{"service"}, tt.args...), &bytes.Buffer{}, &stderr, r.launcher())
			if code != tt.wantCode || (r.service != nil) != (tt.wantCode == 0) {
				t.Fatalf("exit code = %d, options = %+v, stderr = %q", code, r.service, stderr.String())
			}
			if r.service != nil && r.service.Capacity != tt.capacity {
				t.Fatalf("capacity = %d, want %d", r.service.Capacity, tt.capacity)
			}
			if r.service != nil && (r.service.BackfillLimit == nil || *r.service.BackfillLimit != tt.backfill) {
				t.Fatalf("backfill limit = %v, want %d", r.service.BackfillLimit, tt.backfill)
			}
		})
	}
}

func TestDispatchServiceLaunchTimeout(t *testing.T) {
	tests := []struct {
		value string
		code  int
	}{
		{value: "7s", code: 0},
		{value: "0s", code: usageExitCode},
		{value: "-1s", code: usageExitCode},
		{value: "1500us", code: usageExitCode},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			var r recorded
			var stderr bytes.Buffer
			code := dispatch([]string{"service", "--launch-timeout", tt.value}, &bytes.Buffer{}, &stderr, r.launcher())
			if code != tt.code {
				t.Fatalf("exit code = %d, want %d; stderr = %q", code, tt.code, stderr.String())
			}
			if (r.service != nil) != (tt.code == 0) {
				t.Fatalf("service options = %+v", r.service)
			}
			if tt.code == 0 && r.service.LaunchTimeout != 7*time.Second {
				t.Fatalf("launch timeout = %v", r.service.LaunchTimeout)
			}
		})
	}
}

func TestDispatchServiceCancelTimeouts(t *testing.T) {
	for _, name := range []string{"--cancel-ack-timeout", "--cancel-completion-timeout"} {
		for _, value := range []string{"7s", "0s", "-1s", "1500us"} {
			t.Run(name+"/"+value, func(t *testing.T) {
				var r recorded
				var stderr bytes.Buffer
				code := dispatch([]string{"service", name, value}, &bytes.Buffer{}, &stderr, r.launcher())
				want := usageExitCode
				if value == "7s" {
					want = 0
				}
				if code != want || (r.service != nil) != (want == 0) {
					t.Fatalf("exit code = %d, options = %+v, stderr = %q", code, r.service, stderr.String())
				}
				if want == 0 && (name == "--cancel-ack-timeout" && r.service.CancelAckTimeout != 7*time.Second ||
					name == "--cancel-completion-timeout" && r.service.CancelCompletionTimeout != 7*time.Second) {
					t.Fatalf("service timeouts = %+v", r.service)
				}
			})
		}
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
