//go:build darwin

package runner

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/boumenot/heavy-lock/apps/tuna/procid"
)

func TestWaitFallbackDisarmsOnlyAfterExit(t *testing.T) {
	cmd := exec.Command("/bin/cat")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r := &runner{child: cmd.Process, childSignalable: true}
	disarmed := make(chan struct{})
	firstLive := make(chan struct{})
	secondPoll := make(chan struct{})
	result := make(chan childExit, 1)
	done := make(chan struct{})
	go func() {
		polls := 0
		result <- waitForCommandUsing(cmd, start, func() {
			r.disarmChild(cmd.Process)
			close(disarmed)
		}, func(string, ...any) {}, func(int) (time.Time, error) {
			return time.Time{}, errors.New("NOTE_EXIT unavailable")
		}, func(pid int) (int64, procid.State, error) {
			polls++
			identity, state, err := procid.Observe(pid)
			if polls == 1 && err == nil && state == procid.Live {
				close(firstLive)
			}
			if polls == 2 {
				close(secondPoll)
			}
			return identity, state, err
		})
		close(done)
	}()
	t.Cleanup(func() {
		_ = stdin.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("command wait did not complete")
		}
	})
	select {
	case <-firstLive:
	case <-time.After(5 * time.Second):
		t.Fatal("fallback did not observe the live child")
	}
	select {
	case <-secondPoll:
	case <-time.After(5 * time.Second):
		t.Fatal("fallback did not continue polling the live child")
	}
	select {
	case <-disarmed:
		t.Fatal("live child was disarmed before exit")
	default:
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.err != nil || got.elapsedMs != nil || got.rootCPUMs == nil {
			t.Fatalf("unexpected wait result: %+v", got)
		}
		select {
		case <-disarmed:
		default:
			t.Fatal("child was reaped before being disarmed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not finish after child exit")
	}
}

func TestRootRuntimeExcludesDelayedOutputDrain(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", "/bin/cat <&3 &")
	cmd.ExtraFiles = []*os.File{read}
	cmd.Stdout = io.Discard
	start := time.Now()
	if err := cmd.Start(); err != nil {
		_ = read.Close()
		_ = write.Close()
		t.Fatal(err)
	}
	_ = read.Close()
	signals := make(chan os.Signal, 1)
	interrupt, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &runner{o: Options{Signals: signals}, child: cmd.Process, childSignalable: true,
		interrupt: interrupt, stopInterrupt: cancel, logf: t.Logf}
	r.routeSignals()
	disarmed := make(chan struct{})
	result := make(chan childExit, 1)
	done := make(chan struct{})
	go func() {
		result <- waitForCommand(cmd, start, func() {
			r.disarmChild(cmd.Process)
			close(disarmed)
		}, t.Logf)
		close(done)
	}()
	t.Cleanup(func() {
		_ = write.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("command wait did not complete")
		}
	})
	select {
	case <-disarmed:
	case <-time.After(5 * time.Second):
		t.Fatal("root did not exit")
	}
	signals <- syscall.SIGTERM
	deadline := time.Now().Add(5 * time.Second)
	for !r.signalled() {
		if time.Now().After(deadline) {
			t.Fatal("runner did not receive the signal")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, interrupted := r.interrupted(); interrupted || interrupt.Err() != nil {
		t.Fatal("signal during output drain was treated as a pre-launch interrupt")
	}
	if err := r.killChild(cmd.Process); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("disarmed child remained signalable: %v", err)
	}
	select {
	case got := <-result:
		t.Fatalf("wait returned while the descendant still held stdout: %+v", got)
	case <-time.After(300 * time.Millisecond):
	}
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.err != nil || got.elapsedMs == nil {
			t.Fatalf("missing root runtime: %+v", got)
		}
		drain := time.Since(start) - time.Duration(*got.elapsedMs)*time.Millisecond
		if drain < 250*time.Millisecond {
			t.Fatalf("root runtime included output drain: runtime=%dms drain=%v", *got.elapsedMs, drain)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not finish after output drained")
	}
}
