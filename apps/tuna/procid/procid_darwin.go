//go:build darwin

package procid

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// sZOMB is the BSD p_stat value for an exited, unreaped process.
const sZOMB = 5

func observe(pid int) (int64, State, error) {
	if pid <= 0 {
		return 0, Absent, fmt.Errorf("invalid pid %d", pid)
	}
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return 0, Absent, fmt.Errorf("sysctl kern.proc.pid.%d: %w", pid, err)
	}
	switch len(procs) {
	case 0:
		return 0, Absent, nil
	case 1:
	default:
		return 0, Absent, fmt.Errorf("sysctl kern.proc.pid.%d returned %d processes", pid, len(procs))
	}
	p := procs[0].Proc
	if int(p.P_pid) != pid {
		return 0, Absent, fmt.Errorf("sysctl kern.proc.pid.%d returned pid %d", pid, p.P_pid)
	}
	start := int64(p.P_starttime.Sec)*1_000_000_000 + int64(p.P_starttime.Usec)*1_000
	if start <= 0 {
		return 0, Absent, fmt.Errorf("pid %d has no start time", pid)
	}
	if p.P_stat == sZOMB {
		return start, Zombie, nil
	}
	return start, Live, nil
}
