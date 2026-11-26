package jobmetrics

import (
	"strings"
	"testing"
	"time"

	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"google.golang.org/protobuf/proto"
)

func value[T any](v T) *T { return &v }

func sampleProcess(id Identity, parent int64, cpu time.Duration, rss int64) Process {
	return Process{Identity: id, ParentPID: parent, CPUTimeNs: value(int64(cpu)), RSSBytes: value(rss)}
}

func TestObservedTreeSummaryUsesPerCoreIntervalsAndCombinedRSS(t *testing.T) {
	root := Identity{PID: 100, StartTimeUnixNs: 10}
	child := Identity{PID: 101, StartTimeUnixNs: 20}
	other := Identity{PID: 102, StartTimeUnixNs: 30}
	a := New(root, nil, false)
	a.Observe(0, Snapshot{Processes: []Process{
		sampleProcess(root, 1, time.Second, 4),
		sampleProcess(child, 100, 500*time.Millisecond, 2),
		sampleProcess(other, 1, time.Hour, 1000),
	}})
	first := a.Summary()
	if first.CpuTimeMs == nil || *first.CpuTimeMs != 1500 ||
		first.PeakCombinedRssBytes == nil || *first.PeakCombinedRssBytes != 6 ||
		first.PeakCpuPercent != nil {
		t.Fatalf("first sample = %+v", first)
	}

	a.Observe(5*time.Second, Snapshot{Processes: []Process{
		sampleProcess(root, 1, 5*time.Second, 2),
		sampleProcess(child, 100, 6500*time.Millisecond, 10),
	}})
	a.Observe(10*time.Second, Snapshot{Processes: []Process{
		sampleProcess(root, 1, 7*time.Second, 10),
		sampleProcess(child, 100, 6500*time.Millisecond, 1),
	}})
	s := a.Summary()
	if s.GetCpuTimeMs() != 13500 || s.PeakCpuPercent == nil || *s.PeakCpuPercent != 200 ||
		s.GetPeakCombinedRssBytes() != 12 || s.SampleCount != 3 {
		t.Fatalf("summary = %+v; want 13.5 CPU seconds, 200%% peak and 12-byte combined RSS", s)
	}
	if s.Completeness != pb.MeasurementCompleteness_MEASUREMENT_COMPLETENESS_PARTIAL ||
		s.GetIncompletenessReason() == "" {
		t.Fatalf("descendant uncertainty was not preserved: %+v", s)
	}

	reused := Identity{PID: child.PID, StartTimeUnixNs: 40}
	a.Observe(15*time.Second, Snapshot{Processes: []Process{
		sampleProcess(root, 1, 8*time.Second, 4),
		sampleProcess(reused, 100, 2*time.Second, 2),
	}})
	if got := a.Summary().GetCpuTimeMs(); got != 16500 {
		t.Fatalf("PID reuse must retain distinct observed CPU counters: got %d ms", got)
	}
}

func TestUnavailableMeasurementsAndEnumerationFailureAreNotZero(t *testing.T) {
	root := Identity{PID: 100, StartTimeUnixNs: 10}
	child := Identity{PID: 101, StartTimeUnixNs: 20}
	a := New(root, nil, false)
	a.Observe(0, Snapshot{Processes: []Process{
		sampleProcess(root, 1, time.Second, 4),
		{Identity: child, ParentPID: 100, Problem: "permission denied"},
	}})
	a.Failure("process enumeration: permission denied")
	s := a.Summary()
	if s.CpuTimeMs == nil || *s.CpuTimeMs != 1000 || s.PeakCombinedRssBytes != nil ||
		s.PeakCpuPercent != nil || !strings.Contains(s.GetIncompletenessReason(), "permission denied") {
		t.Fatalf("partial sample = %+v", s)
	}

	empty := New(root, nil, false)
	empty.Failure("process enumeration: permission denied")
	s = empty.Summary()
	if s.CpuTimeMs != nil || s.PeakCombinedRssBytes != nil || s.PeakCpuPercent != nil ||
		s.Completeness != pb.MeasurementCompleteness_MEASUREMENT_COMPLETENESS_UNAVAILABLE {
		t.Fatalf("unavailable sample = %+v", s)
	}
}

func TestUnstableParentDoesNotAttributeNewChildren(t *testing.T) {
	root := Identity{PID: 100, StartTimeUnixNs: 10}
	child := Identity{PID: 101, StartTimeUnixNs: 20}
	a := New(root, nil, false)
	a.Observe(0, Snapshot{Processes: []Process{
		{Identity: root, ParentPID: 1, CPUTimeNs: value(int64(time.Hour)), RSSBytes: value(int64(1000)),
			Unstable: true, Problem: "identity changed"},
		sampleProcess(child, 100, time.Hour, 1000),
	}})
	s := a.Summary()
	if s.CpuTimeMs != nil || s.PeakCombinedRssBytes != nil {
		t.Fatalf("unstable ancestry attributed another process's measurements: %+v", s)
	}
}

func TestUnstableChildDoesNotBecomeKnown(t *testing.T) {
	root := Identity{PID: 100, StartTimeUnixNs: 10}
	child := Identity{PID: 101, StartTimeUnixNs: 20}
	a := New(root, nil, false)
	a.Observe(0, Snapshot{Processes: []Process{
		sampleProcess(root, 1, time.Second, 4),
		{Identity: child, ParentPID: 100, Unstable: true, Problem: "identity could not be verified"},
	}})
	a.Observe(5*time.Second, Snapshot{Processes: []Process{
		sampleProcess(root, 1, time.Second, 4),
		sampleProcess(child, 999, time.Hour, 1000),
	}})
	if got := a.Summary().GetCpuTimeMs(); got != 1000 {
		t.Fatalf("unverified child identity attributed unrelated CPU: %d ms", got)
	}
}

func TestRecoveredSummaryKeepsCheckpointsWithoutDoubleCounting(t *testing.T) {
	root := Identity{PID: 100, StartTimeUnixNs: 10}
	previous := &pb.ResourceSummary{
		CpuTimeMs: proto.Int64(10000), PeakCpuPercent: proto.Float64(150),
		PeakCombinedRssBytes: proto.Int64(12), SampleCount: 5,
		Completeness:         pb.MeasurementCompleteness_MEASUREMENT_COMPLETENESS_PARTIAL,
		IncompletenessReason: proto.String("previous limitation"),
	}
	a := New(root, previous, true)
	a.Observe(0, Snapshot{Processes: []Process{sampleProcess(root, 1, 7*time.Second, 4)}})
	if got := a.Summary().GetCpuTimeMs(); got != 10000 {
		t.Fatalf("recovery counted a previously observed counter twice: %d ms", got)
	}
	a.Observe(5*time.Second, Snapshot{Processes: []Process{sampleProcess(root, 1, 9*time.Second, 8)}})
	s := a.Summary()
	if s.GetCpuTimeMs() != 12000 || s.GetPeakCpuPercent() != 150 ||
		s.GetPeakCombinedRssBytes() != 12 || s.SampleCount != 7 ||
		!strings.Contains(s.GetIncompletenessReason(), "restart") {
		t.Fatalf("recovered summary = %+v", s)
	}

	short := New(root, nil, false)
	short.Finish(value(int64(250)), value(int64(12)))
	s = short.Summary()
	if s.ElapsedMs == nil || *s.ElapsedMs != 250 || s.CpuTimeMs == nil || *s.CpuTimeMs != 12 ||
		s.PeakCombinedRssBytes != nil || s.PeakCpuPercent != nil {
		t.Fatalf("short job summary = %+v", s)
	}
}
