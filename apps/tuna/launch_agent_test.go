package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func renderAgent(args ...string) ([]byte, error) {
	return exec.Command("/bin/sh", append([]string{"../../tool/render-tuna-launch-agent.sh"}, args...)...).CombinedOutput()
}

func plistValue(t *testing.T, path, key string) string {
	t.Helper()
	out, err := exec.Command("/usr/bin/plutil", "-extract", key, "raw", "-o", "-", path).CombinedOutput()
	if err != nil {
		t.Fatalf("read %s: %v: %s", key, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestLaunchAgentConfiguration(t *testing.T) {
	binary := "/tmp/a & b/tuna"
	log := "/tmp/a & b/broker.log"
	for _, tc := range []struct {
		name string
		args []string
		port string
	}{
		{"default port", []string{binary, log}, "49783"},
		{"explicit port", []string{binary, log, "50000"}, "50000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, err := renderAgent(tc.args...)
			if err != nil {
				t.Fatalf("render: %v: %s", err, output)
			}
			path := filepath.Join(t.TempDir(), "agent.plist")
			if err := os.WriteFile(path, output, 0o600); err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]string{
				"Label":              "com.github.boumenot.tuna",
				"ProgramArguments.0": binary, "ProgramArguments.1": "service",
				"ProgramArguments.2": "--port", "ProgramArguments.3": tc.port,
				"StandardErrorPath": log, "RunAtLoad": "true", "KeepAlive": "true",
				"ThrottleInterval": "30", "Umask": "63",
			} {
				if got := plistValue(t, path, key); got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
		})
	}
}

func TestLaunchAgentRejectsInvalidConfiguration(t *testing.T) {
	for _, args := range [][]string{
		{}, {"relative", "/tmp/log"}, {"/tmp/tuna", "relative"},
		{"/tmp/tuna", "/tmp/log", "0"}, {"/tmp/tuna", "/tmp/log", "65536"},
		{"/tmp/tuna", "/tmp/log", "x"}, {"/tmp/tuna", "/tmp/log", ""},
		{"/tmp/tuna", "/tmp/log", "10", "extra"}, {"/tmp/tuna", "/tmp/tuna"},
	} {
		if output, err := renderAgent(args...); err == nil {
			t.Errorf("render %v succeeded: %s", args, output)
		}
	}
}
