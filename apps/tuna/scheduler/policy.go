// Package scheduler implements the normal-admission policy and queue
// controls on top of transactional storage. It decides and commits
// reservations; it does not launch, signal or release processes.
package scheduler

import (
	pb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
)

// Entry is one waiting job in durable queue order.
type Entry struct {
	JobID       string
	Weight      uint32
	Priority    int32
	Connected   bool // registered by the current stream of its runner
	BypassCount uint32
}

// Sharing is the result of the live sharing checks (settling, free memory
// and CPU load) for admitting work alongside an occupied reservation.
type Sharing struct {
	Allowed bool
	Reason  string
}

// State is everything one normal-admission decision may depend on.
type State struct {
	Capacity      uint32
	BackfillLimit uint32
	Ready         bool
	Paused        bool // queue mode is PAUSED
	// Occupied includes forced and unreconciled reservations and may exceed
	// Capacity.
	Occupied uint32
	Queue    []Entry
	// Sharing is evaluated lazily, at most once, and only when the candidate
	// would not run alone.
	Sharing func() Sharing
}

// Decision is the single next normal admission, if any, and the reason the
// effective head is not being admitted.
type Decision struct {
	Admit        string
	BackfillHead string // set when Admit bypasses a capacity-blocked head
	Head         string // effective head: earliest job that fits capacity
	HeadBlocked  pb.BlockedReason
	Sharing      *Sharing // evaluated sharing result, when consulted
}

// Decide applies weighted priority order with bounded capacity-only backfill. Oversized
// jobs are skipped and never become the head; a disconnected head blocks;
// a disconnected or oversized follower is skipped and keeps its place.
func Decide(s State) Decision {
	var d Decision
	head := -1
	for i, e := range s.Queue {
		if !Oversized(e.Weight, s.Capacity) {
			head = i
			break
		}
	}
	if head < 0 {
		// No effective head: oversized jobs report their own reason.
		return d
	}
	h := s.Queue[head]
	d.Head = h.JobID
	switch {
	case !s.Ready:
		d.HeadBlocked = pb.BlockedReason_BLOCKED_REASON_RECOVERING
		return d
	case s.Paused:
		d.HeadBlocked = pb.BlockedReason_BLOCKED_REASON_PAUSED
		return d
	case !h.Connected:
		d.HeadBlocked = pb.BlockedReason_BLOCKED_REASON_RUNNER_DISCONNECTED
		return d
	}
	var free uint32
	if s.Occupied < s.Capacity {
		free = s.Capacity - s.Occupied
	}
	sharingAllowed := func() bool {
		// A job running alone is exempt from the sharing checks.
		if s.Occupied == 0 {
			return true
		}
		if d.Sharing == nil {
			result := Sharing{Reason: "sharing checks unavailable"}
			if s.Sharing != nil {
				result = s.Sharing()
			}
			d.Sharing = &result
		}
		return d.Sharing.Allowed
	}
	if h.Weight <= free {
		if sharingAllowed() {
			d.Admit = h.JobID
			return d
		}
		d.HeadBlocked = pb.BlockedReason_BLOCKED_REASON_RESOURCE_CHECK
		return d
	}
	d.HeadBlocked = pb.BlockedReason_BLOCKED_REASON_INSUFFICIENT_SLOTS
	if h.BypassCount >= s.BackfillLimit {
		return d
	}
	for _, e := range s.Queue[head+1:] {
		if Oversized(e.Weight, s.Capacity) || !e.Connected || e.Weight > free {
			continue
		}
		// Sharing checks are not weight-specific: if the earliest eligible
		// follower fails them, every later follower would too.
		if sharingAllowed() {
			d.Admit = e.JobID
			d.BackfillHead = h.JobID
		}
		return d
	}
	return d
}

// QueueState derives the operational queue state from s and its decision d,
// with the contract's precedence after ERROR, which the broker reports only
// when durable state cannot be read: RECOVERING, PAUSED, BLOCKED, RUNNING,
// IDLE. A blocked head is not BLOCKED while backfill admits a follower.
func QueueState(s State, d Decision) pb.QueueState {
	switch {
	case !s.Ready:
		return pb.QueueState_QUEUE_STATE_RECOVERING
	case s.Paused:
		return pb.QueueState_QUEUE_STATE_PAUSED
	case len(s.Queue) > 0 && d.Admit == "":
		return pb.QueueState_QUEUE_STATE_BLOCKED
	case s.Occupied > 0 || d.Admit != "":
		return pb.QueueState_QUEUE_STATE_RUNNING
	}
	return pb.QueueState_QUEUE_STATE_IDLE
}

// Oversized reports a queued job that normal admission skips because a
// capacity decrease across restarts left its weight above capacity.
func Oversized(weight, capacity uint32) bool { return weight > capacity }
