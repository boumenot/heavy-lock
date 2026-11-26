package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrototypeDispatch(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		code       int
		wantOutput string
		wantError  string
	}{
		{name: "help", args: []string{"--help"}, code: 0, wantOutput: "Usage: tuna"},
		{name: "short help", args: []string{"-h"}, code: 0, wantOutput: "Usage: tuna"},
		{name: "missing command", code: 64, wantError: "Usage: tuna"},
		{name: "empty command", args: []string{"--"}, code: 64, wantError: "Usage: tuna"},
		{name: "service", args: []string{"service"}, code: 70, wantError: "tuna: service mode is not implemented"},
		{name: "runner", args: []string{"--port", "49783", "--", "echo", "hello"}, code: 70, wantError: "tuna: runner mode is not implemented"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := run(tt.args, &stdout, &stderr); got != tt.code {
				t.Fatalf("exit code = %d, want %d", got, tt.code)
			}
			if !strings.Contains(stdout.String(), tt.wantOutput) {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantOutput)
			}
			if !strings.Contains(stderr.String(), tt.wantError) {
				t.Errorf("stderr = %q, want %q", stderr.String(), tt.wantError)
			}
			if tt.wantOutput == "" && stdout.Len() != 0 {
				t.Errorf("unexpected stdout = %q", stdout.String())
			}
			if tt.wantError == "" && stderr.Len() != 0 {
				t.Errorf("unexpected stderr = %q", stderr.String())
			}
		})
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

func TestRunnerPrototypeDoesNotLaunchCommand(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	var stdout, stderr bytes.Buffer
	if got := run([]string{"--port", "49783", "--", "/usr/bin/touch", marker}, &stdout, &stderr); got != 70 {
		t.Fatalf("exit code = %d, want 70", got)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prototype must not launch commands; marker stat error = %v", err)
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
		{name: "valid", args: []string{"--port", "49783", "--", "echo"}, code: 70},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := run(tt.args, &stdout, &stderr); got != tt.code {
				t.Fatalf("exit code = %d, want %d; stderr = %q", got, tt.code, stderr.String())
			}
		})
	}
}
