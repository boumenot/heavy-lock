package runner

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/boumenot/heavy-lock/apps/tuna/contract"
	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"github.com/boumenot/heavy-lock/apps/tuna/procid"
)

// maxFreezeRounds bounds the process-table passes that stop newly found
// descendants before the tree is killed.
const maxFreezeRounds = 10

// errTreeUnsettled reports descendants that kept appearing for every freeze
// pass; some may have escaped the kill.
var errTreeUnsettled = errors.New("descendants were still appearing after the last freeze pass")

type treeID struct{ pid, start int64 }

// stopTree cancels a running command and its descendants. It stops the
// unreaped root through the reap fence, then stops each live descendant
// found by parent PID until a pass finds none new, and finally kills the
// descendants and the root. Descendant signals are fingerprint-checked
// (procid.Signal); one that no longer matches is skipped. A descendant that
// left the tree before it was observed, or whose parent exited first, is not
// found here; the broker still holds the reservation until every process it
// tracks is verified exited. Returns os.ErrProcessDone, as killChild does,
// when the root is already disarmed, and otherwise the joined failures.
func (r *runner) stopTree(root *os.Process, rootID *pb.ProcessIdentity) error {
	if err := r.signalChild(root, syscall.SIGSTOP); err != nil {
		return err
	}
	rootStart, err := contract.UnixNano(rootID.GetStartTime())
	if err != nil {
		return errors.Join(fmt.Errorf("root identity: %w", err), r.killChild(root))
	}
	known := map[treeID]bool{{rootID.GetPid(), rootStart}: true}
	var stopped []*pb.ProcessIdentity
	var failures []error
	settled := false
	for range maxFreezeRounds {
		entries, err := procid.Table()
		if err != nil {
			failures = append(failures, fmt.Errorf("read the process table: %w", err))
			settled = true
			break
		}
		found := descendants(entries, known)
		if len(found) == 0 {
			settled = true
			break
		}
		for _, id := range found {
			known[id] = true
			identity := &pb.ProcessIdentity{Pid: id.pid, StartTime: contract.Timestamp(id.start)}
			switch err := procid.Signal(identity, syscall.SIGSTOP); {
			case err == nil:
				stopped = append(stopped, identity)
			case !errors.Is(err, procid.ErrMismatch):
				failures = append(failures, fmt.Errorf("stop descendant %d: %w", id.pid, err))
			}
		}
	}
	if !settled {
		failures = append(failures, errTreeUnsettled)
	}
	for _, identity := range stopped {
		if err := procid.Signal(identity, syscall.SIGKILL); err != nil && !errors.Is(err, procid.ErrMismatch) {
			failures = append(failures, fmt.Errorf("kill descendant %d: %w", identity.Pid, err))
		}
	}
	if err := r.killChild(root); err != nil && !errors.Is(err, os.ErrProcessDone) {
		failures = append(failures, fmt.Errorf("kill the command: %w", err))
	}
	return errors.Join(failures...)
}

// descendants returns live, not-yet-known children of known processes. A
// parent is followed only while its table entry still has the known start
// time, and a child must start no earlier than its parent, so a reused PID
// does not pull an unrelated tree in.
func descendants(entries []procid.Entry, known map[treeID]bool) []treeID {
	byPID := make(map[int64]procid.Entry, len(entries))
	for _, e := range entries {
		byPID[e.PID] = e
	}
	var found []treeID
	for _, e := range entries {
		id := treeID{e.PID, e.StartTimeUnixNs}
		if e.Zombie || e.StartTimeUnixNs <= 0 || known[id] {
			continue
		}
		parent, ok := byPID[e.ParentPID]
		if !ok || !known[treeID{parent.PID, parent.StartTimeUnixNs}] || e.StartTimeUnixNs < parent.StartTimeUnixNs {
			continue
		}
		found = append(found, id)
	}
	return found
}
