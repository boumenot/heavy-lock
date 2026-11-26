package runner

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"time"

	"github.com/boumenot/heavy-lock/apps/tuna/procid"
)

type childExit struct {
	state     *os.ProcessState
	elapsedMs *int64
	rootCPUMs *int64
	err       error
	timingErr error
}

func waitForCommand(cmd *exec.Cmd, start time.Time, beforeWait func(), logf func(string, ...any)) childExit {
	return waitForCommandUsing(cmd, start, beforeWait, logf, observeRootExit, procid.Observe)
}

func waitForCommandUsing(cmd *exec.Cmd, start time.Time, beforeWait func(), logf func(string, ...any), observeExit func(int) (time.Time, error), observeProcess func(int) (int64, procid.State, error)) childExit {
	exitedAt, timingErr := observeExit(cmd.Process.Pid)
	if timingErr != nil {
		logf("root exit notification unavailable; waiting for process evidence before reaping: %v", timingErr)
		waitUntilExited(cmd.Process.Pid, logf, observeProcess)
	}
	beforeWait()
	err := cmd.Wait()
	result := childExit{state: cmd.ProcessState, err: err, timingErr: timingErr}
	if timingErr == nil {
		elapsed := exitedAt.Sub(start)
		if elapsed < 0 {
			result.timingErr = fmt.Errorf("monotonic exit observation precedes start")
		} else {
			result.elapsedMs = ptr(elapsed.Milliseconds())
		}
	}
	if result.state != nil {
		user, system := result.state.UserTime(), result.state.SystemTime()
		if user >= 0 && system >= 0 && user <= time.Duration(math.MaxInt64)-system {
			result.rootCPUMs = ptr((user + system).Milliseconds())
		}
	}
	return result
}

func waitUntilExited(pid int, logf func(string, ...any), observe func(int) (int64, procid.State, error)) {
	reported := false
	for {
		_, state, err := observe(pid)
		if err == nil && (state == procid.Absent || state == procid.Zombie) {
			return
		}
		if err != nil && !reported {
			logf("could not verify command exit before reaping: %v", err)
			reported = true
		}
		time.Sleep(100 * time.Millisecond)
	}
}
