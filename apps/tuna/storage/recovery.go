package storage

import (
	"errors"

	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
)

type Process struct {
	Identity                     *pb.ProcessIdentity
	Role                         string
	FirstSeenAt                  int64
	LastSeenAt, ExitedVerifiedAt *int64
}

// Processes includes retained exit evidence as well as live recovery state.
func (r *Reader) Processes(id string) ([]Process, error) {
	if _, err := r.Job(id); err != nil {
		return nil, err
	}
	rows, err := r.q.QueryContext(r.ctx, `SELECT pid,start_time_ns,role,first_seen_at_ns,
		last_seen_at_ns,exited_verified_at_ns FROM job_processes WHERE job_id=? ORDER BY pid,start_time_ns`, id)
	if err != nil {
		r.err = errors.Join(r.err, err)
		return nil, err
	}
	var processes []Process
	for rows.Next() {
		p := Process{Identity: &pb.ProcessIdentity{}}
		if err = rows.Scan(&p.Identity.Pid, &p.Identity.StartTimeUnixNs, &p.Role, &p.FirstSeenAt, &p.LastSeenAt, &p.ExitedVerifiedAt); err != nil {
			break
		}
		processes = append(processes, p)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	r.err = errors.Join(r.err, err)
	return processes, err
}

func (r *Reader) ActiveJobs() ([]*pb.Job, error) {
	ids, err := r.ids("SELECT id FROM jobs WHERE state IN ('reserved','running','finishing') ORDER BY id")
	if err != nil {
		return nil, err
	}
	var jobs []*pb.Job
	for _, id := range ids {
		j, err := r.Job(id)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}

type Grace struct {
	ElapsedUS               int64
	StartedAt, ServiceRunID *int64
	Runner                  *pb.RunnerIncarnation
	Connected               bool
}

func (r *Reader) QueueGrace(id string) (*Grace, error) {
	g := &Grace{Runner: &pb.RunnerIncarnation{Process: &pb.ProcessIdentity{}}}
	err := r.scan(`SELECT grace_elapsed_us,grace_started_at_ns,grace_service_run_id,
		runner_incarnation_id,runner_pid,runner_start_time_ns,runner_connected
		FROM queue_entries WHERE job_id=?`, []any{id}, &g.ElapsedUS, &g.StartedAt, &g.ServiceRunID,
		&g.Runner.IncarnationId, &g.Runner.Process.Pid, &g.Runner.Process.StartTimeUnixNs, &g.Connected)
	return g, err
}

type ServiceRun struct {
	ID                 int64
	Version            string
	PID, StartedAt     int64
	ReadyAt, StoppedAt *int64
	CleanShutdown      bool
}

func (r *Reader) ServiceRuns() ([]ServiceRun, error) {
	rows, err := r.q.QueryContext(r.ctx, "SELECT id,service_version,pid,started_at_ns,ready_at_ns,stopped_at_ns,clean_shutdown FROM service_runs ORDER BY id")
	if err != nil {
		r.err = errors.Join(r.err, err)
		return nil, err
	}
	var runs []ServiceRun
	for rows.Next() {
		var run ServiceRun
		if err = rows.Scan(&run.ID, &run.Version, &run.PID, &run.StartedAt, &run.ReadyAt, &run.StoppedAt, &run.CleanShutdown); err != nil {
			break
		}
		runs = append(runs, run)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	r.err = errors.Join(r.err, err)
	return runs, err
}
