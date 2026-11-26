package storage

import (
	"database/sql"
	"errors"
	"math"
	"strings"

	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"google.golang.org/protobuf/proto"
)

type Reservation struct {
	GrantID                                   string
	Runner                                    *pb.RunnerIncarnation
	ClaimedAt, FencedAt, ReleasedAt           *int64
	FenceReason, NoLaunchProof, ReleaseReason *string
}

func (r *Reader) Reservation(id string) (*Reservation, error) {
	res := &Reservation{Runner: &pb.RunnerIncarnation{Process: &pb.ProcessIdentity{}}}
	err := r.scan(`SELECT grant_id,runner_incarnation_id,runner_pid,runner_start_time_ns,
		claimed_at_ns,fenced_at_ns,fence_reason,no_launch_proof,released_at_ns,release_reason
		FROM reservations WHERE job_id=?`, []any{id}, &res.GrantID, &res.Runner.IncarnationId,
		&res.Runner.Process.Pid, &res.Runner.Process.StartTimeUnixNs,
		&res.ClaimedAt, &res.FencedAt, &res.FenceReason, &res.NoLaunchProof, &res.ReleasedAt, &res.ReleaseReason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return res, err
}

func (tx *Tx) assigned(id, grant string, runner *pb.RunnerIncarnation) (*pb.Job, *Reservation, error) {
	j, err := tx.Job(id)
	if err != nil {
		return nil, nil, err
	}
	res, err := tx.Reservation(id)
	if err != nil {
		return nil, nil, err
	}
	if grant != res.GrantID || !validRunner(runner) || !proto.Equal(runner, res.Runner) {
		return nil, nil, ErrIdentity
	}
	return j, res, nil
}

func (tx *Tx) Claim(id, grant string, runner *pb.RunnerIncarnation) (alreadyClaimed bool, err error) {
	j, res, err := tx.assigned(id, grant, runner)
	if err != nil {
		return false, err
	}
	if res.ClaimedAt != nil {
		return true, nil
	}
	if j.State != pb.JobState_JOB_STATE_RESERVED || j.CancelRequestedAtUnixNs != nil || res.FencedAt != nil || res.ReleasedAt != nil {
		return false, ErrTransition
	}
	if err := tx.exec("UPDATE reservations SET claimed_at_ns=? WHERE job_id=?", tx.at, id); err != nil {
		return false, err
	}
	return false, tx.jobChanged(id)
}

func (tx *Tx) Seal(id, grant string, runner *pb.RunnerIncarnation, noSpawn bool) error {
	j, res, err := tx.assigned(id, grant, runner)
	if err != nil {
		return err
	}
	if noSpawn && (j.StartedAtUnixNs != nil || j.Process != nil ||
		j.Outcome != nil && (j.Outcome.Kind == pb.OutcomeKind_OUTCOME_KIND_EXITED || j.Outcome.Kind == pb.OutcomeKind_OUTCOME_KIND_SIGNALED)) {
		return ErrIdentity
	}
	if res.FencedAt != nil {
		// Preserve earlier, stronger fencing evidence; only add a no-launch
		// attestation when it does not conflict with a recorded spawn.
		if !noSpawn || res.NoLaunchProof != nil {
			return nil
		}
		if err := tx.exec("UPDATE reservations SET no_launch_proof='runner_confirmed' WHERE job_id=?", id); err != nil {
			return err
		}
	} else {
		var proof any
		if noSpawn {
			proof = "runner_confirmed"
		}
		if err := tx.exec(`UPDATE reservations SET fenced_at_ns=?,fence_reason='runner_sealed',
			no_launch_proof=? WHERE job_id=?`, tx.at, proof, id); err != nil {
			return err
		}
	}
	return tx.jobChanged(id)
}

// FenceRunnerTerminated requires OS-verified death of the assigned incarnation
// from the caller. Disconnection, timeout and an attempted kill are not proof.
func (tx *Tx) FenceRunnerTerminated(id string, runner *pb.RunnerIncarnation) error {
	res, err := tx.Reservation(id)
	if err != nil {
		return err
	}
	if !validRunner(runner) || !proto.Equal(runner, res.Runner) {
		return ErrIdentity
	}
	if res.FencedAt != nil {
		return nil
	}
	if err := tx.exec("UPDATE reservations SET fenced_at_ns=?,fence_reason='runner_terminated' WHERE job_id=?", tx.at, id); err != nil {
		return err
	}
	return tx.jobChanged(id)
}

func validProcess(p *pb.ProcessIdentity) bool { return p != nil && p.Pid > 0 }

func (tx *Tx) started(j *pb.Job, res *Reservation, p *pb.ProcessIdentity) error {
	if !validProcess(p) || res.ClaimedAt == nil || res.NoLaunchProof != nil {
		return ErrIdentity
	}
	if j.Outcome != nil && (j.Outcome.Kind == pb.OutcomeKind_OUTCOME_KIND_LAUNCH_FAILED ||
		j.Outcome.Kind == pb.OutcomeKind_OUTCOME_KIND_CANCELLED_BEFORE_START) {
		return ErrIdentity
	}
	if j.Process != nil {
		if !proto.Equal(j.Process, p) {
			return ErrIdentity
		}
		return nil
	}
	if j.State == pb.JobState_JOB_STATE_FINISHED && (j.Outcome == nil || j.Outcome.Kind != pb.OutcomeKind_OUTCOME_KIND_UNKNOWN) {
		return ErrTransition
	}
	state := "running"
	if j.State == pb.JobState_JOB_STATE_FINISHED {
		state = "finished"
	}
	if err := tx.exec("UPDATE jobs SET state=?,started_at_ns=? WHERE id=?", state, p.StartTimeUnixNs, j.JobId); err != nil {
		return err
	}
	return tx.TrackProcess(j.JobId, p, "command")
}

func (tx *Tx) ReportStarted(id, grant string, runner *pb.RunnerIncarnation, p *pb.ProcessIdentity) error {
	j, res, err := tx.assigned(id, grant, runner)
	if err != nil {
		return err
	}
	if j.Process != nil && proto.Equal(j.Process, p) {
		return nil
	}
	if err := tx.started(j, res, p); err != nil {
		return err
	}
	return tx.jobChanged(id)
}

func (tx *Tx) ReportFinished(id, grant string, runner *pb.RunnerIncarnation, p *pb.ProcessIdentity, outcome *pb.Outcome) error {
	if outcome == nil || !(outcome.Kind == pb.OutcomeKind_OUTCOME_KIND_EXITED && outcome.ExitCode != nil && outcome.SignalNumber == nil && outcome.SignalName == nil ||
		outcome.Kind == pb.OutcomeKind_OUTCOME_KIND_SIGNALED && outcome.SignalNumber != nil && *outcome.SignalNumber > 0) {
		return errors.New("a valid native exited or signaled outcome is required")
	}
	if outcome.ExitCode != nil && (*outcome.ExitCode < -2147483648 || *outcome.ExitCode > 4294967295) {
		return errors.New("exit code is outside the supported native range")
	}
	j, res, err := tx.assigned(id, grant, runner)
	if err != nil {
		return err
	}
	if j.Outcome != nil && j.Outcome.Kind != pb.OutcomeKind_OUTCOME_KIND_UNKNOWN {
		if !proto.Equal(j.Outcome, outcome) || !proto.Equal(j.Process, p) {
			return ErrIdentity
		}
		return nil
	}
	if err := tx.started(j, res, p); err != nil {
		return err
	}
	state := "finishing"
	if j.State == pb.JobState_JOB_STATE_FINISHED {
		state = "finished"
	}
	if err := tx.exec("UPDATE jobs SET state=?,outcome_kind=?,exit_code=?,signal_number=?,signal_name=?,outcome_detail=? WHERE id=?",
		state, strings.ToLower(strings.TrimPrefix(outcome.Kind.String(), "OUTCOME_KIND_")), outcome.ExitCode, outcome.SignalNumber, outcome.SignalName, outcome.Detail, id); err != nil {
		return err
	}
	return tx.jobChanged(id)
}

func (tx *Tx) ReportLaunchFailed(id, grant string, runner *pb.RunnerIncarnation, detail string, cancelled bool) error {
	j, res, err := tx.assigned(id, grant, runner)
	if err != nil {
		return err
	}
	kind := pb.OutcomeKind_OUTCOME_KIND_LAUNCH_FAILED
	if cancelled {
		kind = pb.OutcomeKind_OUTCOME_KIND_CANCELLED_BEFORE_START
	}
	if j.Outcome != nil && j.Outcome.Kind != pb.OutcomeKind_OUTCOME_KIND_UNKNOWN {
		if j.Outcome.Kind == kind && j.Outcome.Detail != nil && *j.Outcome.Detail == detail {
			return nil
		}
		return ErrIdentity
	}
	if j.StartedAtUnixNs != nil || j.Process != nil || res.ClaimedAt == nil || cancelled && j.CancelRequestedAtUnixNs == nil {
		return ErrTransition
	}
	if cancelled {
		if err := tx.acknowledge(j); err != nil {
			return err
		}
	}
	// A no-launch report is an outcome, not permission to free capacity.
	if err := tx.exec("UPDATE jobs SET outcome_kind=?,outcome_detail=? WHERE id=?",
		strings.ToLower(strings.TrimPrefix(kind.String(), "OUTCOME_KIND_")), detail, id); err != nil {
		return err
	}
	return tx.jobChanged(id)
}

func (tx *Tx) TrackProcess(id string, p *pb.ProcessIdentity, role string) error {
	if !validProcess(p) || role != "runner" && role != "command" && role != "descendant" {
		return errors.New("valid process identity and role are required")
	}
	j, err := tx.Job(id)
	if err != nil {
		return err
	}
	if role == "command" && (j.StartedAtUnixNs == nil || *j.StartedAtUnixNs != p.StartTimeUnixNs) ||
		role == "descendant" && (j.State == pb.JobState_JOB_STATE_QUEUED || j.State == pb.JobState_JOB_STATE_FINISHED) {
		return ErrTransition
	}
	var existing *string
	err = tx.scan("SELECT role FROM job_processes WHERE job_id=? AND pid=? AND start_time_ns=?", []any{id, p.Pid, p.StartTimeUnixNs}, &existing)
	if err == nil {
		if *existing != role {
			return ErrIdentity
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if role == "command" {
		var count int
		if err := tx.scan("SELECT count(*) FROM job_processes WHERE job_id=? AND role='command'", []any{id}, &count); err != nil {
			return err
		}
		if count > 0 {
			return ErrIdentity
		}
	}
	return tx.exec("INSERT INTO job_processes(job_id,pid,start_time_ns,role,first_seen_at_ns) VALUES (?,?,?,?,?)", id, p.Pid, p.StartTimeUnixNs, role, tx.at)
}

// VerifyProcessExited persists OS evidence supplied by the caller, matching
// both PID and start time. Neither a reported root outcome nor timeout calls it.
func (tx *Tx) VerifyProcessExited(id string, p *pb.ProcessIdentity) error {
	if !validProcess(p) {
		return ErrIdentity
	}
	var exited *int64
	err := tx.scan("SELECT exited_verified_at_ns FROM job_processes WHERE job_id=? AND pid=? AND start_time_ns=?",
		[]any{id, p.Pid, p.StartTimeUnixNs}, &exited)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrIdentity
	}
	if err != nil || exited != nil {
		return err
	}
	return tx.exec("UPDATE job_processes SET exited_verified_at_ns=? WHERE job_id=? AND pid=? AND start_time_ns=?", tx.at, id, p.Pid, p.StartTimeUnixNs)
}

// Release rechecks durable fencing and all tracked non-runner processes.
// The caller must also establish that discovery is complete and no untracked
// descendants can appear; the database cannot prove OS discovery completeness.
func (tx *Tx) Release(id, reason string) error {
	j, err := tx.Job(id)
	if err != nil {
		return err
	}
	res, err := tx.Reservation(id)
	if err != nil {
		return err
	}
	if res.ReleasedAt != nil {
		if *res.ReleaseReason == reason {
			return nil
		}
		return ErrIdentity
	}
	if res.FencedAt == nil {
		return ErrUnreconciled
	}
	var live bool
	if err := tx.scan("SELECT EXISTS(SELECT 1 FROM job_processes WHERE job_id=? AND role!='runner' AND exited_verified_at_ns IS NULL)", []any{id}, &live); err != nil {
		return err
	}
	if live {
		return ErrUnreconciled
	}
	switch reason {
	case "processes_exited":
		if j.Outcome == nil || j.Outcome.Kind != pb.OutcomeKind_OUTCOME_KIND_EXITED && j.Outcome.Kind != pb.OutcomeKind_OUTCOME_KIND_SIGNALED || j.Process == nil {
			return ErrUnreconciled
		}
	case "launch_failed", "cancelled_before_start":
		if res.NoLaunchProof == nil || j.Process != nil || reason == "cancelled_before_start" && j.CancelRequestedAtUnixNs == nil {
			return ErrUnreconciled
		}
		if reason == "launch_failed" && (j.Outcome == nil || j.Outcome.Kind != pb.OutcomeKind_OUTCOME_KIND_LAUNCH_FAILED) ||
			reason == "cancelled_before_start" && res.ClaimedAt != nil && (j.Outcome == nil || j.Outcome.Kind != pb.OutcomeKind_OUTCOME_KIND_CANCELLED_BEFORE_START) {
			return ErrUnreconciled
		}
		if err := tx.exec("UPDATE jobs SET state='finished',finished_at_ns=?,outcome_kind=?,exit_code=NULL,signal_number=NULL,signal_name=NULL WHERE id=?",
			tx.at, reason, id); err != nil {
			return err
		}
	case "recovery_verified_gone":
		if j.Outcome == nil || j.Outcome.Kind != pb.OutcomeKind_OUTCOME_KIND_UNKNOWN {
			return ErrTransition
		}
	default:
		return errors.New("invalid release reason")
	}
	if err := tx.exec("UPDATE reservations SET released_at_ns=?,release_reason=? WHERE job_id=?", tx.at, reason, id); err != nil {
		return err
	}
	if j.State != pb.JobState_JOB_STATE_FINISHED {
		if err := tx.exec("UPDATE jobs SET state='finished',finished_at_ns=? WHERE id=?", tx.at, id); err != nil {
			return err
		}
	}
	if err := tx.jobChanged(id); err != nil {
		return err
	}
	return tx.statusChanged()
}

func (tx *Tx) Cancel(id string, reason *string) error {
	j, err := tx.Job(id)
	if err != nil {
		return err
	}
	if j.State == pb.JobState_JOB_STATE_FINISHED {
		return ErrTransition
	}
	if j.CancelRequestedAtUnixNs != nil {
		return nil
	}
	if err := tx.exec("UPDATE jobs SET cancel_requested_at_ns=?,cancel_reason=? WHERE id=?", tx.at, reason, id); err != nil {
		return err
	}
	if j.State == pb.JobState_JOB_STATE_QUEUED {
		if err := tx.exec("DELETE FROM queue_entries WHERE job_id=?", id); err != nil {
			return err
		}
		if err := tx.exec("UPDATE jobs SET state='finished',finished_at_ns=?,outcome_kind='cancelled_before_start' WHERE id=?", tx.at, id); err != nil {
			return err
		}
		if err := tx.queueChanged(); err != nil {
			return err
		}
	} else {
		res, err := tx.Reservation(id)
		if err != nil {
			return err
		}
		if res.ClaimedAt == nil {
			if err := tx.exec(`UPDATE reservations SET fenced_at_ns=?,fence_reason='claim_prevented',
				no_launch_proof='claim_prevented' WHERE job_id=?`, tx.at, id); err != nil {
				return err
			}
			return tx.Release(id, "cancelled_before_start")
		}
	}
	return tx.jobChanged(id)
}

func (tx *Tx) acknowledge(j *pb.Job) error {
	if j.CancelRequestedAtUnixNs == nil {
		return ErrTransition
	}
	if j.CancelAcknowledgedAtUnixNs != nil {
		return nil
	}
	return tx.exec("UPDATE jobs SET cancel_acknowledged_at_ns=? WHERE id=?", tx.at, j.JobId)
}

func (tx *Tx) AcknowledgeCancel(id, grant string, runner *pb.RunnerIncarnation) error {
	j, res, err := tx.assigned(id, grant, runner)
	if err != nil {
		return err
	}
	if res.ClaimedAt == nil {
		return ErrTransition
	}
	if j.CancelAcknowledgedAtUnixNs != nil {
		return nil
	}
	if err := tx.acknowledge(j); err != nil {
		return err
	}
	return tx.jobChanged(id)
}

func (tx *Tx) ReportCancelFailed(id, grant string, runner *pb.RunnerIncarnation, detail string) error {
	if detail == "" {
		return errors.New("cancellation failure detail is required")
	}
	j, res, err := tx.assigned(id, grant, runner)
	if err != nil {
		return err
	}
	if res.ClaimedAt == nil {
		return ErrTransition
	}
	if j.CancelFailureDetail != nil {
		if *j.CancelFailureDetail == detail {
			return nil
		}
		return ErrIdentity
	}
	if err := tx.acknowledge(j); err != nil {
		return err
	}
	if err := tx.exec("UPDATE jobs SET cancel_failure_detail=? WHERE id=?", detail, id); err != nil {
		return err
	}
	return tx.jobChanged(id)
}

// RecordTermination records the caller's already-evaluated takeover reason.
// Deadline policy and safe coordination before signalling remain outside storage.
func (tx *Tx) RecordTermination(id string, reason pb.BrokerTermination_Reason) error {
	if reason < pb.BrokerTermination_REASON_CANCEL_ACK_DEADLINE || reason > pb.BrokerTermination_REASON_RUNNER_DEAD {
		return ErrTransition
	}
	j, err := tx.Job(id)
	if err != nil {
		return err
	}
	if j.BrokerTermination != nil {
		// The first takeover reason is sticky: only an identical repeat is a no-op.
		if j.BrokerTermination.Reason == reason {
			return nil
		}
		return ErrIdentity
	}
	if j.AdmittedAtUnixNs == nil || j.State == pb.JobState_JOB_STATE_FINISHED {
		return ErrTransition
	}
	if reason != pb.BrokerTermination_REASON_RUNNER_DEAD && j.CancelRequestedAtUnixNs == nil ||
		reason == pb.BrokerTermination_REASON_CANCEL_ACK_DEADLINE && j.CancelAcknowledgedAtUnixNs != nil ||
		reason == pb.BrokerTermination_REASON_CANCEL_COMPLETION_DEADLINE && j.CancelAcknowledgedAtUnixNs == nil ||
		reason == pb.BrokerTermination_REASON_CANCEL_RUNNER_FAILED && j.CancelFailureDetail == nil {
		return ErrTransition
	}
	if err := tx.exec("UPDATE jobs SET broker_termination_at_ns=?,broker_termination_reason=? WHERE id=?", tx.at,
		strings.ToLower(strings.TrimPrefix(reason.String(), "REASON_")), id); err != nil {
		return err
	}
	return tx.jobChanged(id)
}

func (tx *Tx) MarkUnknown(id string, detail *string) error {
	j, err := tx.Job(id)
	if err != nil {
		return err
	}
	if j.State == pb.JobState_JOB_STATE_QUEUED || j.State == pb.JobState_JOB_STATE_FINISHED ||
		j.Outcome != nil && j.Outcome.Kind != pb.OutcomeKind_OUTCOME_KIND_UNKNOWN {
		return ErrTransition
	}
	if err := tx.exec("UPDATE jobs SET outcome_kind='unknown',outcome_detail=? WHERE id=?", detail, id); err != nil {
		return err
	}
	return tx.jobChanged(id)
}

func (tx *Tx) SaveResources(id string, s *pb.ResourceSummary) error {
	if s == nil || s.Completeness < pb.MeasurementCompleteness_MEASUREMENT_COMPLETENESS_COMPLETE ||
		s.Completeness > pb.MeasurementCompleteness_MEASUREMENT_COMPLETENESS_UNAVAILABLE ||
		s.ElapsedMs != nil && *s.ElapsedMs < 0 || s.CpuTimeMs != nil && *s.CpuTimeMs < 0 ||
		s.PeakCpuPercent != nil && (*s.PeakCpuPercent < 0 || math.IsNaN(*s.PeakCpuPercent) || math.IsInf(*s.PeakCpuPercent, 0)) ||
		s.PeakCombinedRssBytes != nil && *s.PeakCombinedRssBytes < 0 {
		return errors.New("invalid resource summary")
	}
	if _, err := tx.Job(id); err != nil {
		return err
	}
	if err := tx.exec(`INSERT INTO job_resource_summaries(job_id,elapsed_ms,cpu_time_ms,peak_cpu_percent,
		peak_combined_rss_bytes,sample_count,measurement_completeness,incompleteness_reason,updated_at_ns)
		VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT(job_id) DO UPDATE SET
		elapsed_ms=excluded.elapsed_ms,cpu_time_ms=excluded.cpu_time_ms,peak_cpu_percent=excluded.peak_cpu_percent,
		peak_combined_rss_bytes=excluded.peak_combined_rss_bytes,sample_count=excluded.sample_count,
		measurement_completeness=excluded.measurement_completeness,incompleteness_reason=excluded.incompleteness_reason,
		updated_at_ns=excluded.updated_at_ns`, id, s.ElapsedMs, s.CpuTimeMs, s.PeakCpuPercent, s.PeakCombinedRssBytes,
		s.SampleCount, strings.ToLower(strings.TrimPrefix(s.Completeness.String(), "MEASUREMENT_COMPLETENESS_")), s.IncompletenessReason, tx.at); err != nil {
		return err
	}
	return tx.jobChanged(id)
}
