// Package jobmetrics observes process resources for historical summaries.
// Metrics do not prove process exit or authorize signaling or release.
package jobmetrics

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/boumenot/tuna/apps/tuna/contract"
	pb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
	"google.golang.org/protobuf/proto"
)

type Identity struct {
	PID             int64
	StartTimeUnixNs int64
}

type Process struct {
	Identity  Identity
	ParentPID int64
	CPUTimeNs *int64
	RSSBytes  *int64
	Problem   string
	Unstable  bool
}

type Snapshot struct {
	Processes            []Process
	IncompletenessReason string
	// IdentityFailure distinguishes an unusable identity table from gaps
	// confined to CPU or memory measurements.
	IdentityFailure string
}

type Source interface {
	Snapshot(context.Context) (Snapshot, error)
}

type counter struct {
	cpu  int64
	seen uint64
}

type Aggregate struct {
	root       Identity
	summary    *pb.ResourceSummary
	known      map[Identity]struct{}
	counters   map[Identity]counter
	reasons    map[string]struct{}
	cpuNs      int64
	cpuKnown   bool
	afterCrash bool
	sequence   uint64
	lastAt     time.Duration
	hasLastAt  bool
}

func New(root Identity, previous *pb.ResourceSummary, recovered bool) *Aggregate {
	a := &Aggregate{
		root: root, summary: &pb.ResourceSummary{}, known: map[Identity]struct{}{root: {}},
		counters: make(map[Identity]counter), reasons: make(map[string]struct{}),
	}
	if previous != nil {
		a.summary = proto.Clone(previous).(*pb.ResourceSummary)
		if previous.CpuTime != nil {
			ms, err := contract.Milliseconds(previous.CpuTime)
			if err == nil && ms >= 0 && ms <= math.MaxInt64/int64(time.Millisecond) {
				a.cpuNs, a.cpuKnown = ms*int64(time.Millisecond), true
			} else {
				a.reason("previous CPU counter cannot be represented")
				a.summary.CpuTime = nil
			}
		}
		if previous.IncompletenessReason != nil {
			a.reason(*previous.IncompletenessReason)
		}
	}
	a.reason("descendant discovery is incomplete; short-lived or reparented processes may be missed")
	if recovered {
		a.reason("broker restart may have lost samples since the last checkpoint")
		a.afterCrash = a.cpuKnown
	}
	return a
}

// Clone preserves the current observations while a tentative finish report
// is committed; a rejected report must not change the live aggregate.
func (a *Aggregate) Clone() *Aggregate {
	copy := *a
	copy.summary = proto.Clone(a.summary).(*pb.ResourceSummary)
	copy.known = make(map[Identity]struct{}, len(a.known))
	for id := range a.known {
		copy.known[id] = struct{}{}
	}
	copy.counters = make(map[Identity]counter, len(a.counters))
	for id, counter := range a.counters {
		copy.counters[id] = counter
	}
	copy.reasons = make(map[string]struct{}, len(a.reasons))
	for reason := range a.reasons {
		copy.reasons[reason] = struct{}{}
	}
	return &copy
}

func (a *Aggregate) reason(reason string) {
	if reason == "" {
		return
	}
	if _, ok := a.reasons[reason]; ok {
		return
	}
	if len(a.reasons) >= 16 {
		a.reasons["additional observation failures omitted"] = struct{}{}
		return
	}
	a.reasons[reason] = struct{}{}
}

// Failure records an unsuccessful sample and prevents computing a peak over
// an interval whose observations are missing.
func (a *Aggregate) Failure(reason string) {
	if reason == "" {
		reason = "resource observation failed"
	}
	a.reason(reason)
	a.hasLastAt = false
}

func (a *Aggregate) tree(snapshot Snapshot) []Process {
	byPID := make(map[int64]Process, len(snapshot.Processes))
	duplicates := make(map[int64]bool)
	children := make(map[int64][]int64)
	for _, p := range snapshot.Processes {
		if p.Identity.PID <= 0 || p.Identity.StartTimeUnixNs <= 0 {
			a.reason("process enumeration returned an invalid identity")
			continue
		}
		if _, exists := byPID[p.Identity.PID]; exists {
			duplicates[p.Identity.PID] = true
			a.reason("process enumeration returned a duplicate PID")
			continue
		}
		byPID[p.Identity.PID] = p
		children[p.ParentPID] = append(children[p.ParentPID], p.Identity.PID)
	}
	var queue []int64
	for id := range a.known {
		p, ok := byPID[id.PID]
		if ok && !duplicates[id.PID] && p.Identity == id {
			queue = append(queue, id.PID)
		} else {
			a.reason("a previously observed process is no longer visible")
		}
	}
	sort.Slice(queue, func(i, j int) bool { return queue[i] < queue[j] })
	seen := make(map[int64]bool)
	var tree []Process
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] || duplicates[pid] {
			continue
		}
		p, ok := byPID[pid]
		if !ok {
			continue
		}
		seen[pid] = true
		tree = append(tree, p)
		if p.Unstable {
			a.reason("unstable ancestry prevented discovery of new descendants")
		} else {
			a.known[p.Identity] = struct{}{}
			queue = append(queue, children[pid]...)
		}
	}
	if len(tree) == 0 {
		a.reason("no known job process was present in the snapshot")
	}
	return tree
}

func (a *Aggregate) addCPU(delta int64) bool {
	if delta < 0 || a.cpuNs > math.MaxInt64-delta {
		a.reason("observed CPU time overflowed")
		return false
	}
	a.cpuNs += delta
	a.cpuKnown = true
	return true
}

// Observe adds one instantaneous process-table snapshot. The supplied time
// is monotonic within this broker run, not a wall-clock timestamp.
func (a *Aggregate) Observe(at time.Duration, snapshot Snapshot) {
	a.reason(snapshot.IncompletenessReason)
	tree := a.tree(snapshot)
	if len(tree) == 0 {
		a.hasLastAt = false
		return
	}
	if a.summary.SampleCount == ^uint32(0) {
		a.reason("sample count reached its representable limit")
	} else {
		a.summary.SampleCount++
	}
	a.sequence++
	var rss, intervalCPU int64
	rssKnown, intervalKnown := true, false
	for _, p := range tree {
		if p.Problem != "" {
			a.reason(fmt.Sprintf("pid %d: %s", p.Identity.PID, p.Problem))
		}
		if p.Unstable {
			rssKnown = false
			a.reason("unstable process measurements were excluded")
			continue
		}
		if p.RSSBytes == nil || *p.RSSBytes < 0 || rss > math.MaxInt64-*p.RSSBytes {
			rssKnown = false
			a.reason("RSS was unavailable for an observed process")
		} else {
			rss += *p.RSSBytes
		}
		if p.CPUTimeNs == nil || *p.CPUTimeNs < 0 {
			a.reason("CPU time was unavailable for an observed process")
			continue
		}
		current := *p.CPUTimeNs
		previous, found := a.counters[p.Identity]
		var delta int64
		switch {
		case !found && !a.afterCrash:
			delta = current
		case found && current >= previous.cpu:
			delta = current - previous.cpu
			if previous.seen+1 == a.sequence && intervalCPU <= math.MaxInt64-delta {
				intervalCPU += delta
				intervalKnown = true
			}
		case found:
			a.reason("an observed process CPU counter decreased")
		}
		a.addCPU(delta)
		a.counters[p.Identity] = counter{cpu: current, seen: a.sequence}
	}
	if rssKnown && (a.summary.PeakCombinedRssBytes == nil || rss > *a.summary.PeakCombinedRssBytes) {
		a.summary.PeakCombinedRssBytes = proto.Int64(rss)
	}
	if a.hasLastAt && at > a.lastAt && intervalKnown {
		peak := float64(intervalCPU) / float64(at-a.lastAt) * 100
		if a.summary.PeakCpuPercent == nil || peak > *a.summary.PeakCpuPercent {
			a.summary.PeakCpuPercent = proto.Float64(peak)
		}
	} else if a.hasLastAt && at <= a.lastAt {
		a.reason("sampling interval was not monotonic")
	}
	a.lastAt, a.hasLastAt = at, at >= 0
}

// Finish incorporates the runner's monotonic root duration and final root
// wait CPU counter. It does not infer a runtime from wall-clock timestamps.
func (a *Aggregate) Finish(elapsedMs, rootCPUMs *int64) {
	if elapsedMs == nil || *elapsedMs < 0 {
		a.reason("root runtime was unavailable")
	} else {
		a.summary.Elapsed = contract.MillisecondDuration(*elapsedMs)
	}
	if rootCPUMs == nil || *rootCPUMs < 0 || *rootCPUMs > math.MaxInt64/int64(time.Millisecond) {
		a.reason("final root CPU time was unavailable")
	} else {
		final := *rootCPUMs * int64(time.Millisecond)
		previous, found := a.counters[a.root]
		switch {
		case found && final >= previous.cpu:
			a.addCPU(final - previous.cpu)
		case found:
			a.reason("final root CPU time was below its sampled counter")
		case a.afterCrash:
			if final > a.cpuNs {
				a.cpuNs = final
			}
			a.reason("root CPU time could not be separated from the recovered total")
		default:
			a.addCPU(final)
		}
	}
	if a.summary.SampleCount == 0 {
		a.reason("no periodic resource sample was recorded")
	}
}

func (a *Aggregate) Summary() *pb.ResourceSummary {
	summary := proto.Clone(a.summary).(*pb.ResourceSummary)
	if a.cpuKnown {
		summary.CpuTime = contract.MillisecondDuration(a.cpuNs / int64(time.Millisecond))
	}
	if summary.Elapsed != nil || summary.CpuTime != nil ||
		summary.PeakCpuPercent != nil || summary.PeakCombinedRssBytes != nil {
		summary.Completeness = pb.MeasurementCompleteness_MEASUREMENT_COMPLETENESS_PARTIAL
	} else {
		summary.Completeness = pb.MeasurementCompleteness_MEASUREMENT_COMPLETENESS_UNAVAILABLE
	}
	reasons := make([]string, 0, len(a.reasons))
	for reason := range a.reasons {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	summary.IncompletenessReason = proto.String(strings.Join(reasons, "; "))
	return summary
}
