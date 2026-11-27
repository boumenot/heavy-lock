package runner

import (
	"slices"
	"testing"

	"github.com/boumenot/heavy-lock/apps/tuna/procid"
)

func TestDescendantsFollowOnlyKnownIncarnations(t *testing.T) {
	known := map[treeID]bool{{10, 100}: true}
	entries := []procid.Entry{
		{PID: 10, StartTimeUnixNs: 100, ParentPID: 1},
		{PID: 11, StartTimeUnixNs: 110, ParentPID: 10},               // child
		{PID: 12, StartTimeUnixNs: 120, ParentPID: 11},               // grandchild: next pass
		{PID: 13, StartTimeUnixNs: 90, ParentPID: 10},                // older than its parent
		{PID: 14, StartTimeUnixNs: 140, ParentPID: 10, Zombie: true}, // already exited
		{PID: 15, StartTimeUnixNs: 0, ParentPID: 10},                 // invalid identity
		{PID: 20, StartTimeUnixNs: 200, ParentPID: 1},
		{PID: 21, StartTimeUnixNs: 210, ParentPID: 20}, // unrelated tree
	}
	if got := descendants(entries, known); !slices.Equal(got, []treeID{{11, 110}}) {
		t.Fatalf("first pass = %v", got)
	}
	known[treeID{11, 110}] = true
	if got := descendants(entries, known); !slices.Equal(got, []treeID{{12, 120}}) {
		t.Fatalf("second pass = %v", got)
	}
	// The root's PID now belongs to another incarnation: its children are not followed.
	entries[0].StartTimeUnixNs = 105
	delete(known, treeID{11, 110})
	if got := descendants(entries, known); len(got) != 0 {
		t.Fatalf("reused root PID pulled in %v", got)
	}
}
