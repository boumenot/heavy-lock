package storage

import (
	"errors"
	"fmt"
	"math"
	"slices"

	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
)

func (tx *Tx) StartService(version string, pid int64) (int64, error) {
	if version == "" || pid <= 0 {
		return 0, errors.New("service version and positive PID are required")
	}
	var id int64
	err := tx.scan("INSERT INTO service_runs(service_version,pid,started_at_ns) VALUES (?,?,?) RETURNING id",
		[]any{version, pid, tx.at}, &id)
	return id, err
}

// ReadyService is called only after the broker trusts its recovered accounting.
// It commits the ready marker and restart snapshots together.
func (tx *Tx) ReadyService(id int64) error {
	var ready, stopped *int64
	if err := tx.scan("SELECT ready_at_ns,stopped_at_ns FROM service_runs WHERE id=?", []any{id}, &ready, &stopped); err != nil {
		return err
	}
	if stopped != nil {
		return ErrTransition
	}
	if ready != nil {
		return nil
	}
	if err := tx.exec("UPDATE service_runs SET ready_at_ns=? WHERE id=?", tx.at, id); err != nil {
		return err
	}
	if err := tx.statusChanged(); err != nil {
		return err
	}
	return tx.queueChanged()
}

func (tx *Tx) StopService(id int64) error {
	var stopped *int64
	if err := tx.scan("SELECT stopped_at_ns FROM service_runs WHERE id=?", []any{id}, &stopped); err != nil {
		return err
	}
	if stopped != nil {
		return nil
	}
	return tx.exec("UPDATE service_runs SET stopped_at_ns=?,clean_shutdown=1 WHERE id=? AND stopped_at_ns IS NULL", tx.at, id)
}

// SetQueueMode durably sets the admission mode. Setting the current mode is
// a no-op; a change commits status and queue snapshots, whose projected
// queue state and head reason depend on it.
func (tx *Tx) SetQueueMode(mode pb.QueueMode) error {
	var value string
	var at any
	switch mode {
	case pb.QueueMode_QUEUE_MODE_ENABLED:
		value = "enabled"
	case pb.QueueMode_QUEUE_MODE_PAUSED:
		value, at = "paused", tx.at
	default:
		return fmt.Errorf("invalid queue mode %v", mode)
	}
	s, err := tx.Status()
	if err != nil || s.QueueMode == mode {
		return err
	}
	if err := tx.exec("UPDATE broker_state SET queue_mode=?,paused_at_ns=? WHERE id=1", value, at); err != nil {
		return err
	}
	if err := tx.statusChanged(); err != nil {
		return err
	}
	return tx.queueChanged()
}

// SetQueueConnection must be invoked only for the current stream registration;
// stream supersession is owned by the broker, not the database.
func (tx *Tx) SetQueueConnection(id string, runner *pb.RunnerIncarnation, connected bool, run int64) error {
	if !validRunner(runner) {
		return ErrIdentity
	}
	j, err := tx.Job(id)
	if err != nil {
		return err
	}
	if j.State != pb.JobState_JOB_STATE_QUEUED {
		return ErrTransition
	}
	var incarnation string
	var pid, start int64
	if err := tx.scan("SELECT runner_incarnation_id,runner_pid,runner_start_time_ns FROM queue_entries WHERE job_id=?",
		[]any{id}, &incarnation, &pid, &start); err != nil {
		return err
	}
	if incarnation != runner.IncarnationId || pid != runner.Process.Pid || start != runner.Process.StartTimeUnixNs {
		return ErrIdentity
	}
	if j.RunnerConnected == connected {
		return nil
	}
	if connected {
		err = tx.exec(`UPDATE queue_entries SET runner_connected=1,grace_elapsed_us=0,
			grace_started_at_ns=NULL,grace_service_run_id=NULL WHERE job_id=?`, id)
	} else {
		err = tx.exec(`UPDATE queue_entries SET runner_connected=0,grace_elapsed_us=0,
			grace_started_at_ns=?,grace_service_run_id=? WHERE job_id=?`, tx.at, run, id)
	}
	if err != nil {
		return err
	}
	if err := tx.jobChanged(id); err != nil {
		return err
	}
	return tx.queueChanged()
}

// CheckpointGrace persists monotonic elapsed time. A new service run changes
// the informational epoch without charging broker downtime.
func (tx *Tx) CheckpointGrace(id string, elapsedUS, run int64) error {
	if elapsedUS < 0 || run <= 0 {
		return errors.New("invalid grace checkpoint")
	}
	var connected bool
	var previous int64
	if err := tx.scan("SELECT runner_connected,grace_elapsed_us FROM queue_entries WHERE job_id=?", []any{id}, &connected, &previous); err != nil {
		return err
	}
	if connected || elapsedUS < previous {
		return ErrTransition
	}

	return tx.exec("UPDATE queue_entries SET grace_elapsed_us=?,grace_started_at_ns=?,grace_service_run_id=? WHERE job_id=?",
		elapsedUS, tx.at, run, id)
}

// RemoveDisconnected rechecks connection state after the broker evaluates the
// grace deadline; a reconnect committed first prevents removal.
func (tx *Tx) RemoveDisconnected(id string) error {
	j, err := tx.Job(id)
	if err != nil {
		return err
	}
	if j.State != pb.JobState_JOB_STATE_QUEUED || j.RunnerConnected {
		return ErrTransition
	}
	if err := tx.exec("DELETE FROM queue_entries WHERE job_id=?", id); err != nil {
		return err
	}
	if err := tx.exec("UPDATE jobs SET state='finished',finished_at_ns=?,outcome_kind='removed_disconnected' WHERE id=?", tx.at, id); err != nil {
		return err
	}
	if err := tx.jobChanged(id); err != nil {
		return err
	}
	return tx.queueChanged()
}

// MoveBefore uses an empty before ID for the back. Negative temporary
// sequences avoid SQLite's per-row UNIQUE collisions during renumbering.
func (tx *Tx) MoveBefore(id, before string) error {
	ids, err := tx.ids("SELECT job_id FROM queue_entries ORDER BY queue_seq")
	if err != nil {
		return err
	}
	from := slices.Index(ids, id)
	if from < 0 {
		return ErrTransition
	}
	if id == before {
		return nil
	}
	ids = slices.Delete(ids, from, from+1)
	to := len(ids)
	if before != "" {
		to = slices.Index(ids, before)
		if to < 0 {
			return ErrTransition
		}
	}
	ids = slices.Insert(ids, to, id)
	var minimum int64
	if err := tx.scan("SELECT min(queue_seq) FROM queue_entries", nil, &minimum); err != nil {
		return err
	}
	base := min(minimum, 0)
	if base < math.MinInt64+int64(len(ids)) {
		return errors.New("queue sequence space exhausted")
	}
	for i, id := range ids {
		if err := tx.exec("UPDATE queue_entries SET queue_seq=? WHERE job_id=?", base-int64(i+1), id); err != nil {
			return err
		}
	}
	for i, id := range ids {
		if err := tx.exec("UPDATE queue_entries SET queue_seq=? WHERE job_id=?", int64(i+1), id); err != nil {
			return err
		}
	}
	if err := tx.exec("UPDATE broker_state SET next_queue_seq=? WHERE id=1", len(ids)+1); err != nil {
		return err
	}
	return tx.queueChanged()
}

type Admission struct {
	Forced          bool
	BackfillHead    string
	FreeMemoryBytes *int64
}

func (tx *Tx) Reserve(id string, run int64, admission Admission) error {
	j, err := tx.Job(id)
	if err != nil {
		return err
	}
	if j.State != pb.JobState_JOB_STATE_QUEUED || !j.RunnerConnected {
		return ErrTransition
	}
	runner := &pb.RunnerIncarnation{Process: &pb.ProcessIdentity{}}
	if err := tx.scan("SELECT runner_incarnation_id,runner_pid,runner_start_time_ns FROM queue_entries WHERE job_id=?",
		[]any{id}, &runner.IncarnationId, &runner.Process.Pid, &runner.Process.StartTimeUnixNs); err != nil {
		return err
	}
	if err := tx.reserve(id, runner, run, admission); err != nil {
		return err
	}
	if err := tx.exec("DELETE FROM queue_entries WHERE job_id=?", id); err != nil {
		return err
	}
	if err := tx.jobChanged(id); err != nil {
		return err
	}
	if err := tx.queueChanged(); err != nil {
		return err
	}
	return tx.statusChanged()
}

func (tx *Tx) reserve(id string, runner *pb.RunnerIncarnation, run int64, admission Admission) error {
	if admission.Forced && admission.BackfillHead != "" || admission.FreeMemoryBytes != nil && *admission.FreeMemoryBytes < 0 {
		return errors.New("invalid admission")
	}
	grant, err := newID()
	if err != nil {
		return err
	}
	if admission.BackfillHead != "" {
		head, err := tx.Job(admission.BackfillHead)
		if err != nil {
			return err
		}
		if head.State != pb.JobState_JOB_STATE_QUEUED || head.JobId == id {
			return ErrTransition
		}
		if err := tx.exec("UPDATE jobs SET backfill_bypass_count=backfill_bypass_count+1 WHERE id=?", head.JobId); err != nil {
			return err
		}
		if err := tx.jobChanged(head.JobId); err != nil {
			return err
		}
	}
	if err := tx.exec(`UPDATE jobs SET state='reserved',admitted_at_ns=?,forced=?,
		admitted_by_backfill=?,free_memory_at_admission_bytes=? WHERE id=?`,
		tx.at, admission.Forced, admission.BackfillHead != "", admission.FreeMemoryBytes, id); err != nil {
		return err
	}
	return tx.exec(`INSERT INTO reservations(job_id,grant_id,weight,forced,granted_at_ns,
		service_run_id,runner_incarnation_id,runner_pid,runner_start_time_ns)
		SELECT id,?,weight,forced,?,?,?,?,? FROM jobs WHERE id=?`,
		grant, tx.at, run, runner.IncarnationId, runner.Process.Pid, runner.Process.StartTimeUnixNs, id)
}
