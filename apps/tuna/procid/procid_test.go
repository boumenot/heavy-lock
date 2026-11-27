package procid

import (
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"

	pb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func requireSupported(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("process identity is only implemented on macOS")
	}
}

func TestSelfIsLiveAndStable(t *testing.T) {
	requireSupported(t)
	a, err := Self()
	if err != nil {
		t.Fatal(err)
	}
	b, err := Self()
	if err != nil || a.StartTime == nil || !proto.Equal(a.StartTime, b.StartTime) || a.StartTime.AsTime().After(time.Now()) {
		t.Fatalf("unstable self identity %v %v: %v", a, b, err)
	}
	if exited, err := Exited(a); err != nil || exited {
		t.Fatalf("self reported exited=%v err=%v", exited, err)
	}
}

func TestChildZombieThenReapedIsExited(t *testing.T) {
	requireSupported(t)
	cmd := exec.Command("/bin/sh", "-c", "exit 3")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	id, err := Of(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	// Wait for the child to become a zombie without reaping it.
	var info syscall.WaitStatus
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, state, err := Observe(cmd.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		if state == Zombie {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child never became a zombie")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if exited, err := Exited(id); err != nil || !exited {
		t.Fatalf("zombie exited=%v err=%v", exited, err)
	}
	if _, err := syscall.Wait4(cmd.Process.Pid, &info, 0, nil); err != nil || info.ExitStatus() != 3 {
		t.Fatalf("reap: %v status %v", err, info)
	}
	if exited, err := Exited(id); err != nil || !exited {
		t.Fatalf("reaped exited=%v err=%v", exited, err)
	}
}

func TestReplacedStartTimeIsExited(t *testing.T) {
	requireSupported(t)
	self, err := Self()
	if err != nil {
		t.Fatal(err)
	}
	other := &pb.ProcessIdentity{Pid: self.Pid, StartTime: timestamppb.New(self.StartTime.AsTime().Add(-time.Nanosecond))}
	if exited, err := Exited(other); err != nil || !exited {
		t.Fatalf("mismatched incarnation exited=%v err=%v", exited, err)
	}
	if _, err := Exited(&pb.ProcessIdentity{Pid: 0}); err == nil {
		t.Fatal("invalid identity accepted")
	}
	if _, err := Exited(&pb.ProcessIdentity{Pid: self.Pid}); err == nil {
		t.Fatal("identity without a start time was treated as exited")
	}
}
