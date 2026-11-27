package procid

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	pb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestFingerprintCheck(t *testing.T) {
	const start, boot, uid = int64(2_000), int64(1_000), uint32(501)
	live := fingerprint{start: start, state: Live, ruid: uid, euid: uid}
	for name, tc := range map[string]struct {
		f        fingerprint
		expected int64
		boot     int64
		match    bool
	}{
		"match":              {live, start, boot, true},
		"absent":             {fingerprint{state: Absent}, start, boot, false},
		"reused PID":         {live, start + 1_000, boot, false},
		"zombie":             {fingerprint{start: start, state: Zombie, ruid: uid, euid: uid}, start, boot, false},
		"previous boot":      {live, start, start + 1, false},
		"real uid changed":   {fingerprint{start: start, state: Live, ruid: 0, euid: uid}, start, boot, false},
		"effective uid diff": {fingerprint{start: start, state: Live, ruid: uid, euid: 0}, start, boot, false},
	} {
		err := tc.f.check(tc.expected, tc.boot, uid, uid)
		if tc.match != (err == nil) || (err != nil && !errors.Is(err, ErrMismatch)) {
			t.Errorf("%s: check() = %v", name, err)
		}
	}
}

func startSleeper(t *testing.T) (*exec.Cmd, *pb.ProcessIdentity) {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	id, err := Of(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	return cmd, id
}

func waitState(t *testing.T, pid int, want State) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, state, err := Observe(pid); err != nil {
			t.Fatal(err)
		} else if state == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d never reached state %d", pid, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSignalKillsMatchingIncarnation(t *testing.T) {
	requireSupported(t)
	cmd, id := startSleeper(t)
	if err := Signal(id, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("child was not killed: %v", err)
	}
}

func TestSignalSkipsOtherIncarnationAndZombie(t *testing.T) {
	requireSupported(t)
	cmd, id := startSleeper(t)
	reused := &pb.ProcessIdentity{Pid: id.Pid, StartTime: timestamppb.New(id.StartTime.AsTime().Add(time.Microsecond))}
	if err := Signal(reused, syscall.SIGKILL); !errors.Is(err, ErrMismatch) {
		t.Fatalf("signal to another incarnation = %v", err)
	}
	if exited, err := Exited(id); err != nil || exited {
		t.Fatalf("mismatched signal reached the process: exited=%v err=%v", exited, err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitState(t, cmd.Process.Pid, Zombie)
	if err := Signal(id, syscall.SIGKILL); !errors.Is(err, ErrMismatch) {
		t.Fatalf("signal to zombie = %v", err)
	}
	if _, err := Exited(&pb.ProcessIdentity{Pid: id.Pid}); err == nil {
		t.Fatal("identity without start time accepted")
	}
	if err := Signal(&pb.ProcessIdentity{Pid: id.Pid}, syscall.SIGKILL); err == nil || errors.Is(err, ErrMismatch) {
		t.Fatalf("invalid identity = %v", err)
	}
}

func TestTableListsChildWithParent(t *testing.T) {
	requireSupported(t)
	_, id := startSleeper(t)
	entries, err := Table()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.PID == id.Pid {
			if e.ParentPID != int64(os.Getpid()) || e.StartTimeUnixNs != id.StartTime.AsTime().UnixNano() || e.Zombie {
				t.Fatalf("child entry %+v, identity %v", e, id)
			}
			return
		}
	}
	t.Fatalf("child %d missing from table", id.Pid)
}
