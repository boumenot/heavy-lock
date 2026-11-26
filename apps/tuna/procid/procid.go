// Package procid observes OS process incarnations: a PID plus its kernel
// start time, so a reused PID is never mistaken for the original process.
package procid

import (
	"context"
	"errors"
	"os"

	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
)

// ErrUnsupported reports that this platform has no verified identity
// primitive. Callers fail closed: no claim is made and no release is proven.
var ErrUnsupported = errors.New("process identity is not supported on this platform")

// State is one observation of a PID.
type State int

const (
	// Absent: no process currently has the PID.
	Absent State = iota
	// Live: a running or stopped process has the PID.
	Live
	// Zombie: the process exited but has not been reaped; it keeps its start time.
	Zombie
)

// Observe returns the PID's current start time and state. Absent processes
// return a zero start time.
func Observe(pid int) (startTimeUnixNs int64, state State, err error) { return observe(pid) }

// Of returns the identity of a live (or not yet reaped) process.
func Of(pid int) (*pb.ProcessIdentity, error) {
	start, state, err := observe(pid)
	if err != nil {
		return nil, err
	}
	if state == Absent {
		return nil, errors.New("process is not present")
	}
	return &pb.ProcessIdentity{Pid: int64(pid), StartTimeUnixNs: start}, nil
}

// Self returns the identity of the calling process.
func Self() (*pb.ProcessIdentity, error) { return Of(os.Getpid()) }

// Exited reports whether the identified incarnation has verifiably exited:
// its PID is absent, now belongs to a different start time, or is a zombie.
// Any observation failure is returned as an error, never as exited.
func Exited(p *pb.ProcessIdentity) (bool, error) {
	if p == nil || p.Pid <= 0 || p.Pid > int64(^uint32(0)>>1) {
		return false, errors.New("a valid process identity is required")
	}
	start, state, err := observe(int(p.Pid))
	if err != nil {
		return false, err
	}
	return state == Absent || state == Zombie || start != p.StartTimeUnixNs, nil
}

// Probe adapts Exited to the scheduler's process probe.
type Probe struct{}

func (Probe) Exited(ctx context.Context, p *pb.ProcessIdentity) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return Exited(p)
}
