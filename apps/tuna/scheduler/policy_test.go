package scheduler

import (
	"testing"

	pb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
)

func TestDecide(t *testing.T) {
	allow := func() Sharing { return Sharing{Allowed: true} }
	deny := func() Sharing { return Sharing{Reason: "busy"} }
	e := func(id string, weight uint32, connected bool, bypass uint32) Entry {
		return Entry{JobID: id, Weight: weight, Connected: connected, BypassCount: bypass}
	}
	cases := []struct {
		name     string
		state    State
		admit    string
		backfill string
		head     string
		blocked  pb.BlockedReason
		queue    pb.QueueState
	}{
		{name: "empty queue", state: State{Capacity: 4, BackfillLimit: 1, Ready: true}, queue: pb.QueueState_QUEUE_STATE_IDLE},
		{name: "fifo head alone skips sharing", state: State{Capacity: 4, BackfillLimit: 1, Ready: true, Sharing: deny,
			Queue: []Entry{e("a", 4, true, 0), e("b", 1, true, 0)}}, admit: "a", head: "a", queue: pb.QueueState_QUEUE_STATE_RUNNING},
		{name: "head shares when allowed", state: State{Capacity: 4, BackfillLimit: 1, Ready: true, Occupied: 2, Sharing: allow,
			Queue: []Entry{e("a", 2, true, 0)}}, admit: "a", head: "a", queue: pb.QueueState_QUEUE_STATE_RUNNING},
		{name: "head resource check blocks without backfill", state: State{Capacity: 4, BackfillLimit: 1, Ready: true, Occupied: 1, Sharing: deny,
			Queue: []Entry{e("a", 2, true, 0), e("b", 1, true, 0)}}, head: "a", blocked: pb.BlockedReason_BLOCKED_REASON_RESOURCE_CHECK, queue: pb.QueueState_QUEUE_STATE_BLOCKED},
		{name: "recovering", state: State{Capacity: 4, BackfillLimit: 1, Sharing: allow,
			Queue: []Entry{e("a", 1, true, 0)}}, head: "a", blocked: pb.BlockedReason_BLOCKED_REASON_RECOVERING, queue: pb.QueueState_QUEUE_STATE_RECOVERING},
		{name: "paused", state: State{Capacity: 4, BackfillLimit: 1, Ready: true, Paused: true, Sharing: allow,
			Queue: []Entry{e("a", 1, true, 0)}}, head: "a", blocked: pb.BlockedReason_BLOCKED_REASON_PAUSED, queue: pb.QueueState_QUEUE_STATE_PAUSED},
		{name: "disconnected head blocks backfill", state: State{Capacity: 4, BackfillLimit: 1, Ready: true, Occupied: 3, Sharing: allow,
			Queue: []Entry{e("a", 2, false, 0), e("b", 1, true, 0)}}, head: "a", blocked: pb.BlockedReason_BLOCKED_REASON_RUNNER_DISCONNECTED, queue: pb.QueueState_QUEUE_STATE_BLOCKED},
		{name: "backfill earliest eligible follower", state: State{Capacity: 4, BackfillLimit: 1, Ready: true, Occupied: 3, Sharing: allow,
			Queue: []Entry{e("a", 2, true, 0), e("b", 2, true, 0), e("c", 1, false, 0), e("d", 1, true, 0), e("e", 1, true, 0)}},
			admit: "d", backfill: "a", head: "a", blocked: pb.BlockedReason_BLOCKED_REASON_INSUFFICIENT_SLOTS, queue: pb.QueueState_QUEUE_STATE_RUNNING},
		{name: "bypass limit reached", state: State{Capacity: 4, BackfillLimit: 1, Ready: true, Occupied: 3, Sharing: allow,
			Queue: []Entry{e("a", 2, true, 1), e("b", 1, true, 0)}}, head: "a", blocked: pb.BlockedReason_BLOCKED_REASON_INSUFFICIENT_SLOTS, queue: pb.QueueState_QUEUE_STATE_BLOCKED},
		{name: "strict fifo", state: State{Capacity: 4, BackfillLimit: 0, Ready: true, Occupied: 3, Sharing: allow,
			Queue: []Entry{e("a", 2, true, 0), e("b", 1, true, 0)}}, head: "a", blocked: pb.BlockedReason_BLOCKED_REASON_INSUFFICIENT_SLOTS, queue: pb.QueueState_QUEUE_STATE_BLOCKED},
		{name: "follower fails sharing stops", state: State{Capacity: 4, BackfillLimit: 1, Ready: true, Occupied: 3, Sharing: deny,
			Queue: []Entry{e("a", 2, true, 0), e("b", 1, true, 0)}}, head: "a", blocked: pb.BlockedReason_BLOCKED_REASON_INSUFFICIENT_SLOTS, queue: pb.QueueState_QUEUE_STATE_BLOCKED},
		{name: "oversized skipped and never head", state: State{Capacity: 2, BackfillLimit: 1, Ready: true, Sharing: allow,
			Queue: []Entry{e("big", 3, true, 0), e("a", 2, true, 0)}}, admit: "a", head: "a", queue: pb.QueueState_QUEUE_STATE_RUNNING},
		{name: "only oversized", state: State{Capacity: 2, BackfillLimit: 1, Ready: true, Sharing: allow,
			Queue: []Entry{e("big", 3, true, 0)}}, queue: pb.QueueState_QUEUE_STATE_BLOCKED},
		{name: "forced occupancy beyond capacity", state: State{Capacity: 2, BackfillLimit: 1, Ready: true, Occupied: 5, Sharing: allow,
			Queue: []Entry{e("a", 2, true, 0), e("b", 1, true, 0)}}, head: "a", blocked: pb.BlockedReason_BLOCKED_REASON_INSUFFICIENT_SLOTS, queue: pb.QueueState_QUEUE_STATE_BLOCKED},
		{name: "running with empty queue", state: State{Capacity: 4, BackfillLimit: 1, Ready: true, Occupied: 2},
			queue: pb.QueueState_QUEUE_STATE_RUNNING},
		{name: "paused while idle", state: State{Capacity: 4, BackfillLimit: 1, Ready: true, Paused: true},
			queue: pb.QueueState_QUEUE_STATE_PAUSED},
		{name: "recovering outranks paused", state: State{Capacity: 4, BackfillLimit: 1, Paused: true, Occupied: 1,
			Queue: []Entry{e("a", 1, true, 0)}}, head: "a", blocked: pb.BlockedReason_BLOCKED_REASON_RECOVERING,
			queue: pb.QueueState_QUEUE_STATE_RECOVERING},
		{name: "paused outranks running", state: State{Capacity: 4, BackfillLimit: 1, Ready: true, Paused: true, Occupied: 4,
			Queue: []Entry{e("a", 1, true, 0)}}, head: "a", blocked: pb.BlockedReason_BLOCKED_REASON_PAUSED,
			queue: pb.QueueState_QUEUE_STATE_PAUSED},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Decide(c.state)
			if d.Admit != c.admit || d.BackfillHead != c.backfill || d.Head != c.head || d.HeadBlocked != c.blocked {
				t.Fatalf("got admit=%q backfill=%q head=%q blocked=%v", d.Admit, d.BackfillHead, d.Head, d.HeadBlocked)
			}
			if got := QueueState(c.state, d); got != c.queue {
				t.Fatalf("queue state %v, want %v", got, c.queue)
			}
		})
	}
}

func TestDecideSamplesSharingAtMostOnceAndOnlyWhenSharing(t *testing.T) {
	calls := 0
	sharing := func() Sharing { calls++; return Sharing{Reason: "busy"} }
	Decide(State{Capacity: 4, BackfillLimit: 1, Ready: true, Sharing: sharing, Queue: []Entry{{JobID: "a", Weight: 1, Connected: true}}})
	if calls != 0 {
		t.Fatalf("job running alone consulted sharing %d times", calls)
	}
	d := Decide(State{Capacity: 4, BackfillLimit: 1, Ready: true, Occupied: 1, Sharing: sharing,
		Queue: []Entry{{JobID: "a", Weight: 1, Connected: true}}})
	if calls != 1 || d.Sharing == nil || d.Sharing.Reason != "busy" {
		t.Fatalf("calls=%d sharing=%v", calls, d.Sharing)
	}
}
