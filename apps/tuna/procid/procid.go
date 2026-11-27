// Package procid observes OS process incarnations: a PID plus its kernel
// start time, so a reused PID is never mistaken for the original process.
package procid

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/boumenot/tuna/apps/tuna/contract"
	pb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
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
	return &pb.ProcessIdentity{Pid: int64(pid), StartTime: contract.Timestamp(start)}, nil
}

// Self returns the identity of the calling process.
func Self() (*pb.ProcessIdentity, error) { return Of(os.Getpid()) }

// Exited reports whether the identified incarnation has verifiably exited:
// its PID is absent, now belongs to a different start time, or is a zombie.
// Any observation failure is returned as an error, never as exited.
func Exited(p *pb.ProcessIdentity) (bool, error) {
	expected, err := expectedStart(p)
	if err != nil {
		return false, err
	}
	start, state, err := observe(int(p.Pid))
	if err != nil {
		return false, err
	}
	return state == Absent || state == Zombie || start != expected, nil
}

func expectedStart(p *pb.ProcessIdentity) (int64, error) {
	if p == nil || p.Pid <= 0 || p.Pid > int64(^uint32(0)>>1) {
		return 0, errors.New("a valid process identity is required")
	}
	expected, err := contract.UnixNano(p.StartTime)
	if err != nil || expected <= 0 {
		return 0, errors.New("a valid process start time is required")
	}
	return expected, nil
}

// ErrMismatch reports that the PID did not carry the recorded fingerprint,
// so nothing was signalled. It is never evidence that the process exited.
var ErrMismatch = errors.New("process does not match its recorded identity")

// ErrRedirected reports that the PID belonged to a different incarnation
// right after the signal: the accepted check-to-signal race may have sent it
// to another process.
var ErrRedirected = errors.New("PID changed incarnation around the signal")

// fingerprint is one observation of the keys checked before a signal. Only
// the PID and start time identify an incarnation; they survive exec. The
// executable, arguments and working directory change legitimately on exec
// and are deliberately not compared.
type fingerprint struct {
	start      int64
	state      State
	ruid, euid uint32
}

func (f fingerprint) check(expected, boot int64, ruid, euid uint32) error {
	switch {
	case f.state == Absent:
		return fmt.Errorf("%w: the PID is absent", ErrMismatch)
	case f.start != expected:
		return fmt.Errorf("%w: the PID has a different start time", ErrMismatch)
	case f.state == Zombie:
		return fmt.Errorf("%w: the process already exited and awaits reaping", ErrMismatch)
	case expected < boot:
		return fmt.Errorf("%w: the recorded start predates the current boot", ErrMismatch)
	case f.ruid != ruid || f.euid != euid:
		return fmt.Errorf("%w: the process credentials differ from this process's", ErrMismatch)
	}
	return nil
}

// Signal sends sig to the identified incarnation only if its fingerprint
// matches immediately before the signal, and observes the PID again
// afterwards. This is a fingerprint-checked best-effort signal, not
// identity-bound signalling: between the last check and kill(2) the process
// can exit, be reaped and have its PID reused, and no bound on that window
// is assumed. A nil error means the signal was sent. Neither a signal, a
// skipped signal nor any error is evidence that the process exited.
func Signal(p *pb.ProcessIdentity, sig syscall.Signal) error {
	expected, err := expectedStart(p)
	if err != nil {
		return err
	}
	boot, err := bootTime()
	if err != nil {
		return fmt.Errorf("read boot time: %w", err)
	}
	pid := int(p.Pid)
	before, err := inspect(pid)
	if err != nil {
		return err
	}
	if err := before.check(expected, boot, uint32(os.Getuid()), uint32(os.Geteuid())); err != nil {
		return err
	}
	if err := kill(pid, sig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("%w: the PID disappeared before the signal", ErrMismatch)
		}
		return fmt.Errorf("signal PID %d: %w", pid, err)
	}
	if after, err := inspect(pid); err == nil && after.state != Absent && after.start != expected {
		return fmt.Errorf("%w: PID %d now has a different start time", ErrRedirected, pid)
	}
	return nil
}

// Entry is one row of the process table.
type Entry struct {
	PID             int64
	StartTimeUnixNs int64 // 0 when the start time is invalid
	ParentPID       int64
	Zombie          bool
}

// Table returns every process visible to this user. Omissions are not
// evidence that a process exited.
func Table() ([]Entry, error) { return table() }

// Terminator adapts Signal to the scheduler's process terminator.
type Terminator struct{}

func (Terminator) Signal(ctx context.Context, p *pb.ProcessIdentity, sig syscall.Signal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return Signal(p, sig)
}

// Probe adapts Exited to the scheduler's process probe.
type Probe struct{}

func (Probe) Exited(ctx context.Context, p *pb.ProcessIdentity) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return Exited(p)
}
