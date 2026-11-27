//go:build darwin

package procid

import (
	"fmt"
	"math"
	"syscall"

	"golang.org/x/sys/unix"
)

// sZOMB is the BSD p_stat value for an exited, unreaped process.
const sZOMB = 5

func observe(pid int) (int64, State, error) {
	f, err := inspect(pid)
	return f.start, f.state, err
}

func inspect(pid int) (fingerprint, error) {
	if pid <= 0 {
		return fingerprint{}, fmt.Errorf("invalid pid %d", pid)
	}
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return fingerprint{}, fmt.Errorf("sysctl kern.proc.pid.%d: %w", pid, err)
	}
	switch len(procs) {
	case 0:
		return fingerprint{state: Absent}, nil
	case 1:
	default:
		return fingerprint{}, fmt.Errorf("sysctl kern.proc.pid.%d returned %d processes", pid, len(procs))
	}
	p := procs[0].Proc
	if int(p.P_pid) != pid {
		return fingerprint{}, fmt.Errorf("sysctl kern.proc.pid.%d returned pid %d", pid, p.P_pid)
	}
	start := startNanos(p.P_starttime)
	if start <= 0 {
		return fingerprint{}, fmt.Errorf("pid %d has no start time", pid)
	}
	f := fingerprint{start: start, state: Live, ruid: procs[0].Eproc.Pcred.P_ruid, euid: procs[0].Eproc.Ucred.Uid}
	if p.P_stat == sZOMB {
		f.state = Zombie
	}
	return f, nil
}

func startNanos(t unix.Timeval) int64 {
	if t.Sec <= 0 || t.Usec < 0 || t.Usec >= 1_000_000 ||
		t.Sec > (math.MaxInt64-int64(t.Usec)*1_000)/1_000_000_000 {
		return 0
	}
	return int64(t.Sec)*1_000_000_000 + int64(t.Usec)*1_000
}

func bootTime() (int64, error) {
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return 0, err
	}
	boot := startNanos(*tv)
	if boot <= 0 {
		return 0, fmt.Errorf("invalid boot time %v", *tv)
	}
	return boot, nil
}

func kill(pid int, sig syscall.Signal) error { return unix.Kill(pid, sig) }

func table() ([]Entry, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(procs))
	for _, process := range procs {
		p := process.Proc
		entries = append(entries, Entry{PID: int64(p.P_pid), StartTimeUnixNs: startNanos(p.P_starttime),
			ParentPID: int64(process.Eproc.Ppid), Zombie: p.P_stat == sZOMB})
	}
	return entries, nil
}
