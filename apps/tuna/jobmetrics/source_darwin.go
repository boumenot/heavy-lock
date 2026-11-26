//go:build darwin

package jobmetrics

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type tableProcess struct {
	identity Identity
	parent   int64
	zombie   bool
}

type DarwinSource struct {
	table func() ([]tableProcess, error)
	ps    func(context.Context) ([]byte, error)
}

func DefaultSource() Source { return DarwinSource{} }

func (s DarwinSource) Snapshot(ctx context.Context) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	table := s.table
	if table == nil {
		table = darwinProcessTable
	}
	ps := s.ps
	if ps == nil {
		ps = darwinPS
	}
	before, err := table()
	if err != nil {
		return Snapshot{}, fmt.Errorf("enumerate processes: %w", err)
	}
	output, psErr := ps(ctx)
	var rows map[int64]psRow
	if psErr == nil {
		rows, psErr = parsePS(output)
	}
	after, afterErr := table()
	var gaps []string
	if psErr != nil {
		gaps = append(gaps, "process metrics: "+psErr.Error())
	}
	if afterErr != nil {
		gaps = append(gaps, "process identity verification: "+afterErr.Error())
	}
	later := make(map[int64]tableProcess, len(after))
	duplicates := make(map[int64]bool)
	for _, p := range after {
		if _, ok := later[p.identity.PID]; ok {
			duplicates[p.identity.PID] = true
		}
		later[p.identity.PID] = p
	}
	result := Snapshot{IncompletenessReason: strings.Join(gaps, "; ")}
	for _, p := range before {
		observation := Process{Identity: p.identity, ParentPID: p.parent}
		current, found := later[p.identity.PID]
		switch {
		case afterErr != nil:
			observation.Problem = "process identity could not be verified"
			observation.Unstable = true
		case !found || duplicates[p.identity.PID] || current.identity != p.identity || current.parent != p.parent:
			observation.Problem = "process identity or parent changed during sampling"
			observation.Unstable = true
		case p.zombie || current.zombie:
			observation.Problem = "process exited during sampling"
			observation.Unstable = true
		case psErr != nil:
			observation.Problem = "process metrics could not be read"
		default:
			row, found := rows[p.identity.PID]
			switch {
			case !found:
				observation.Problem = "process metrics were omitted"
			case row.parent != p.parent:
				observation.Problem = "process parent changed during sampling"
				observation.Unstable = true
			default:
				observation.CPUTimeNs, observation.RSSBytes, observation.Problem = row.cpuNs, row.rssBytes, row.problem
			}
		}
		result.Processes = append(result.Processes, observation)
	}
	return result, nil
}

func darwinProcessTable() ([]tableProcess, error) {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	result := make([]tableProcess, 0, len(processes))
	for _, process := range processes {
		p := process.Proc
		var start int64
		if p.P_starttime.Sec > 0 && p.P_starttime.Usec >= 0 && p.P_starttime.Usec < 1_000_000 &&
			p.P_starttime.Sec <= (math.MaxInt64-int64(p.P_starttime.Usec)*1_000)/1_000_000_000 {
			start = int64(p.P_starttime.Sec)*1_000_000_000 + int64(p.P_starttime.Usec)*1_000
		}
		result = append(result, tableProcess{
			identity: Identity{PID: int64(p.P_pid), StartTimeUnixNs: start},
			parent:   int64(process.Eproc.Ppid),
			zombie:   p.P_stat == 5,
		})
	}
	return result, nil
}

func darwinPS(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "/bin/ps", "-A", "-o", "pid=", "-o", "ppid=", "-o", "time=", "-o", "rss=")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	return output, nil
}

type psRow struct {
	parent   int64
	cpuNs    *int64
	rssBytes *int64
	problem  string
}

func parsePS(output []byte) (map[int64]psRow, error) {
	rows := make(map[int64]psRow)
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 4 {
			return nil, errors.New("ps returned a malformed process row")
		}
		pid, pidErr := strconv.ParseInt(fields[0], 10, 64)
		parent, parentErr := strconv.ParseInt(fields[1], 10, 64)
		if pidErr != nil || parentErr != nil || pid <= 0 || parent < 0 {
			return nil, errors.New("ps returned an invalid PID or parent PID")
		}
		if _, exists := rows[pid]; exists {
			return nil, errors.New("ps returned a duplicate PID")
		}
		row := psRow{parent: parent}
		cpu, cpuErr := parseCPUTime(fields[2])
		if cpuErr == nil {
			row.cpuNs = &cpu
		} else {
			row.problem = "CPU time unavailable: " + cpuErr.Error()
		}
		rss, rssErr := strconv.ParseInt(fields[3], 10, 64)
		if rssErr == nil && rss >= 0 && rss <= math.MaxInt64/1024 {
			bytes := rss * 1024
			row.rssBytes = &bytes
		} else {
			if row.problem != "" {
				row.problem += "; "
			}
			row.problem += "RSS unavailable"
		}
		rows[pid] = row
	}
	return rows, nil
}

func parseCPUTime(raw string) (int64, error) {
	fields := strings.Split(raw, ":")
	if len(fields) != 2 && len(fields) != 3 {
		return 0, errors.New("invalid ps CPU time")
	}
	parseWhole := func(v string) (int64, error) {
		if v == "" || strings.Trim(v, "0123456789") != "" {
			return 0, errors.New("invalid ps CPU time")
		}
		return strconv.ParseInt(v, 10, 64)
	}
	var hours int64
	if len(fields) == 3 {
		hourFields := strings.Split(fields[0], "-")
		if len(hourFields) > 2 {
			return 0, errors.New("invalid ps CPU time")
		}
		if len(hourFields) == 2 {
			days, err := parseWhole(hourFields[0])
			if err != nil || days > math.MaxInt64/24 {
				return 0, errors.New("invalid ps CPU time")
			}
			hours = days * 24
		}
		hour, err := parseWhole(hourFields[len(hourFields)-1])
		if err != nil || len(hourFields) == 2 && hour >= 24 || hours > math.MaxInt64-hour {
			return 0, errors.New("invalid ps CPU time")
		}
		hours += hour
	}
	minutes, err := parseWhole(fields[len(fields)-2])
	if err != nil || len(fields) == 3 && minutes >= 60 || hours > (math.MaxInt64-minutes)/60 {
		return 0, errors.New("invalid ps CPU time")
	}
	totalMinutes := hours*60 + minutes
	secondsField := fields[len(fields)-1]
	if secondsField == "" || strings.Trim(secondsField, "0123456789.") != "" || strings.Count(secondsField, ".") > 1 {
		return 0, errors.New("invalid ps CPU time")
	}
	seconds, err := time.ParseDuration(secondsField + "s")
	if err != nil || seconds < 0 || seconds >= time.Minute || totalMinutes > (math.MaxInt64-int64(seconds))/int64(time.Minute) {
		return 0, errors.New("invalid ps CPU time")
	}
	return totalMinutes*int64(time.Minute) + int64(seconds), nil
}
