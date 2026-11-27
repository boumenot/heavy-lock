//go:build darwin

package jobmetrics

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/boumenot/heavy-lock/apps/tuna/procid"
)

func tableSequence(tables ...[]tableProcess) func() ([]tableProcess, error) {
	n := 0
	return func() ([]tableProcess, error) {
		if n >= len(tables) {
			return nil, errors.New("unexpected table read")
		}
		table := tables[n]
		n++
		return table, nil
	}
}

func TestDarwinSnapshotVerifiesIdentityAndParsesMetrics(t *testing.T) {
	root := Identity{PID: 100, StartTimeUnixNs: 10}
	child := Identity{PID: 101, StartTimeUnixNs: 20}
	before := []tableProcess{{identity: root, parent: 1}, {identity: child, parent: 100}}
	source := DarwinSource{
		table: tableSequence(before, before),
		ps: func(context.Context) ([]byte, error) {
			return []byte("100 1 1:02.30 1024\n101 100 0:00.50 512\n"), nil
		},
	}
	snapshot, err := source.Snapshot(context.Background())
	if err != nil || len(snapshot.Processes) != 2 || snapshot.IncompletenessReason != "" {
		t.Fatalf("snapshot = %+v, %v", snapshot, err)
	}
	p := snapshot.Processes[0]
	if p.Identity != root || p.ParentPID != 1 || p.CPUTimeNs == nil ||
		*p.CPUTimeNs != int64(62300*time.Millisecond) || p.RSSBytes == nil || *p.RSSBytes != 1024*1024 || p.Problem != "" {
		t.Fatalf("root observation = %+v", p)
	}
}

func TestDarwinSnapshotReportsRacesAndPartialFailures(t *testing.T) {
	root := Identity{PID: 100, StartTimeUnixNs: 10}
	child := Identity{PID: 101, StartTimeUnixNs: 20}
	before := []tableProcess{{identity: root, parent: 1}, {identity: child, parent: 100}}
	after := []tableProcess{{identity: root, parent: 1}, {identity: Identity{PID: 101, StartTimeUnixNs: 30}, parent: 100}}
	source := DarwinSource{
		table: tableSequence(before, after),
		ps: func(context.Context) ([]byte, error) {
			return []byte("100 1 0:01.00 10\n101 100 0:02.00 20\n"), nil
		},
	}
	snapshot, err := source.Snapshot(context.Background())
	if err != nil || len(snapshot.Processes) != 2 {
		t.Fatalf("snapshot = %+v, %v", snapshot, err)
	}
	p := snapshot.Processes[1]
	if p.Identity != child || p.CPUTimeNs != nil || p.RSSBytes != nil || p.Problem == "" || !p.Unstable {
		t.Fatalf("reused PID must not receive another process's metrics: %+v", p)
	}

	source = DarwinSource{
		table: tableSequence(before, before),
		ps:    func(context.Context) ([]byte, error) { return nil, errors.New("permission denied") },
	}
	snapshot, err = source.Snapshot(context.Background())
	if err != nil || !strings.Contains(snapshot.IncompletenessReason, "permission denied") ||
		snapshot.IdentityFailure != "" ||
		len(snapshot.Processes) != 2 || snapshot.Processes[0].CPUTimeNs != nil ||
		snapshot.Processes[0].RSSBytes != nil {
		t.Fatalf("permission failure = %+v, %v", snapshot, err)
	}

	calls := 0
	source.table = func() ([]tableProcess, error) {
		calls++
		if calls == 1 {
			return before, nil
		}
		return nil, errors.New("verification denied")
	}
	snapshot, err = source.Snapshot(context.Background())
	if err != nil || !strings.Contains(snapshot.IdentityFailure, "verification denied") ||
		len(snapshot.Processes) != 2 || !snapshot.Processes[0].Unstable ||
		!snapshot.Processes[1].Unstable {
		t.Fatalf("identity verification failure = %+v, %v", snapshot, err)
	}

	source.table = tableSequence(before, nil)
	snapshot, err = source.Snapshot(context.Background())
	if err != nil || snapshot.IdentityFailure == "" {
		t.Fatalf("empty verification table = %+v, %v", snapshot, err)
	}
	source.table = tableSequence(nil)
	if _, err := source.Snapshot(context.Background()); err == nil {
		t.Fatal("empty initial process table was accepted")
	}

	source.table = func() ([]tableProcess, error) { return nil, errors.New("enumeration denied") }
	if _, err := source.Snapshot(context.Background()); err == nil || !strings.Contains(err.Error(), "enumeration denied") {
		t.Fatalf("initial enumeration failure = %v", err)
	}
}

func TestDarwinSnapshotObservesSelf(t *testing.T) {
	identity, err := procid.Self()
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := DefaultSource().Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range snapshot.Processes {
		if p.Identity.PID != int64(os.Getpid()) {
			continue
		}
		if p.Identity.StartTimeUnixNs != identity.StartTime.AsTime().UnixNano() || p.Problem != "" ||
			p.CPUTimeNs == nil || p.RSSBytes == nil || *p.RSSBytes <= 0 {
			t.Fatalf("self observation = %+v; snapshot gap = %q", p, snapshot.IncompletenessReason)
		}
		return
	}
	t.Fatal("snapshot omitted the current process")
}

func TestDarwinSnapshotObservesLiveDescendant(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", "/bin/cat <&3 & echo $!; wait")
	cmd.ExtraFiles = []*os.File{read}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = read.Close()
		_ = write.Close()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		_ = read.Close()
		_ = write.Close()
		t.Fatal(err)
	}
	_ = read.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = write.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("wait for workload: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("workload did not exit")
		}
	})
	type lineResult struct {
		line string
		err  error
	}
	line := make(chan lineResult, 1)
	go func() {
		value, err := bufio.NewReader(stdout).ReadString('\n')
		line <- lineResult{value, err}
	}()
	var childPID int
	select {
	case result := <-line:
		if result.err != nil {
			t.Fatal(result.err)
		}
		childPID, err = strconv.Atoi(strings.TrimSpace(result.line))
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("workload did not report its child")
	}
	root, err := procid.Of(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	rootID := Identity{PID: root.Pid, StartTimeUnixNs: root.StartTime.AsTime().UnixNano()}
	deadline := time.Now().Add(5 * time.Second)
	for {
		snapshot, err := DefaultSource().Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var rootRSS, childRSS *int64
		for _, process := range snapshot.Processes {
			if process.Identity == rootID && !process.Unstable && process.CPUTimeNs != nil {
				rootRSS = process.RSSBytes
			}
			if process.Identity.PID == int64(childPID) && process.ParentPID == root.Pid &&
				!process.Unstable && process.CPUTimeNs != nil {
				childRSS = process.RSSBytes
			}
		}
		if rootRSS != nil && childRSS != nil && *rootRSS > 0 && *childRSS > 0 {
			aggregate := New(rootID, nil, false)
			aggregate.Observe(0, snapshot)
			if got := aggregate.Summary().GetPeakCombinedRssBytes(); got != *rootRSS+*childRSS {
				t.Fatalf("combined RSS = %d, want root %d + child %d", got, *rootRSS, *childRSS)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("did not observe both live parent and child with resource measurements")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func BenchmarkDarwinSnapshot(b *testing.B) {
	source := DefaultSource()
	ctx := context.Background()
	var processes int
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		snapshot, err := source.Snapshot(ctx)
		if err != nil {
			b.Fatal(err)
		}
		if snapshot.IncompletenessReason != "" {
			b.Fatal(snapshot.IncompletenessReason)
		}
		processes = len(snapshot.Processes)
	}
	b.ReportMetric(float64(processes), "processes")
}
