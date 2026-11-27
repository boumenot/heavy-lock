package storage

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/boumenot/heavy-lock/apps/tuna/contract"
	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var extraPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

func validRunner(r *pb.RunnerIncarnation) bool {
	return r != nil && uuidPattern.MatchString(r.IncarnationId) && validProcess(r.Process)
}

func validateSubmission(req *pb.SubmitJobRequest) error {
	if req == nil || !uuidPattern.MatchString(req.IdempotencyKey) {
		return errors.New("canonical UUIDv4 idempotency key is required")
	}
	if req.Command == "" || !utf8.ValidString(req.Command) || strings.ContainsRune(req.Command, 0) || req.Weight == 0 {
		return errors.New("nonempty UTF-8 command without NUL and positive weight are required")
	}
	if !validRunner(req.Runner) {
		return errors.New("runner incarnation and positive process identity are required")
	}
	if req.RetryOfJobId != nil && !uuidPattern.MatchString(*req.RetryOfJobId) {
		return errors.New("retry predecessor must be a canonical UUIDv4")
	}
	if req.Client != nil {
		if _, ok := pb.ClientOS_name[int32(req.Client.Os)]; !ok {
			return errors.New("unsupported client OS")
		}
		if _, ok := pb.ClientArchitecture_name[int32(req.Client.Architecture)]; !ok {
			return errors.New("unsupported client architecture")
		}
		if v := req.Client.OsVersion; v != nil && (*v == "" || len(*v) > 64 || !utf8.ValidString(*v)) {
			return errors.New("present client OS version must be nonempty UTF-8 of at most 64 bytes")
		}
	}
	if a := req.Attribution; a != nil {
		for _, value := range []*string{a.Repository, a.Branch, a.Worklog, a.AgentSession,
			a.Purpose, a.CommitSha, a.BuildTarget, a.BuildConfiguration} {
			if value != nil && (*value == "" || !utf8.ValidString(*value)) {
				return errors.New("present attribution fields must be nonempty UTF-8")
			}
		}
		if a.CommitSha != nil && (len(*a.CommitSha) < 7 || len(*a.CommitSha) > 64) {
			return errors.New("commit SHA must be 7..64 characters")
		}
		for key, value := range a.Extra {
			// Explicit fields and Job fields are reserved, including snake_case
			// names. The protobuf descriptors keep this in sync with the contract.
			if !extraPattern.MatchString(key) || len(value) > 1024 || !utf8.ValidString(value) ||
				a.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(key)) != nil ||
				(&pb.Job{}).ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(key)) != nil {
				return fmt.Errorf("invalid or reserved extra metadata key %q", key)
			}
			for _, reserved := range []string{"id", "request_sha256", "metadata_json", "client_name", "client_version",
				"client_os", "client_os_version", "client_architecture", "retry_submitted", "state", "outcome_kind", "runner",
				"queued_at_ns", "admitted_at_ns", "started_at_ns", "finished_at_ns", "cancel_requested_at_ns"} {
				if key == reserved {
					return fmt.Errorf("reserved extra metadata key %q", key)
				}
			}
		}
		if len(a.Extra) > 0 {
			metadata, err := json.Marshal(a.Extra)
			if err != nil || len(metadata) > 16384 {
				return errors.Join(errors.New("extra metadata exceeds 16 KiB or cannot be encoded"), err)
			}
		}
	}
	return nil
}

// Submission describes already-decided admission, not scheduling policy.
// No-wait with nil Admission records a committed busy key; non-nil Admission
// commits a reservation. Policy must be evaluated inside the same Write.
type Submission struct {
	Mode           contract.AdmissionMode
	ServiceVersion string
	ServiceRunID   int64
	Admission      *Admission
}

type SubmissionResult struct {
	Job     *pb.Job
	Created bool
	Busy    bool
}

func (tx *Tx) Submit(req *pb.SubmitJobRequest, options Submission) (SubmissionResult, error) {
	var result SubmissionResult
	if req == nil || !uuidPattern.MatchString(req.IdempotencyKey) {
		return result, errors.New("canonical UUIDv4 idempotency key is required")
	}
	existing, err := tx.Submission(req.IdempotencyKey)
	if err == nil {
		hash, hashErr := contract.CanonicalSubmitHash(req, options.Mode)
		if hashErr != nil {
			return result, hashErr
		}
		var stored []byte
		if err := tx.scan("SELECT request_sha256 FROM jobs WHERE id = ?", []any{existing.JobId}, &stored); err != nil {
			return result, err
		}
		if !bytes.Equal(hash[:], stored) {
			return result, ErrKeyReused
		}
		result.Job = existing
		return result, nil
	}
	if errors.Is(err, ErrAdmissionBusy) {
		result.Busy = true
		return result, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return result, err
	}
	if err := validateSubmission(req); err != nil {
		return result, err
	}
	hash, err := contract.CanonicalSubmitHash(req, options.Mode)
	if err != nil {
		return result, err
	}
	if options.ServiceVersion == "" || options.ServiceRunID <= 0 {
		return result, errors.New("service version and run are required")
	}
	if options.Mode == contract.AdmissionQueued && options.Admission != nil {
		return result, errors.New("queued submissions cannot include an admission")
	}
	if options.Mode == contract.AdmissionNoWait && options.Admission != nil && options.Admission.Forced {
		return result, errors.New("no-wait submissions cannot bypass normal admission")
	}
	attempt := 1
	if req.RetryOfJobId != nil {
		var state, outcome string
		var code, released *int64
		var cancelled *int64
		var forced, submitted bool
		err := tx.scan(`SELECT j.state, coalesce(j.outcome_kind,''), j.exit_code,
			j.forced, j.cancel_requested_at_ns, j.attempt, j.retry_submitted, r.released_at_ns
			FROM jobs j LEFT JOIN reservations r ON r.job_id=j.id WHERE j.id=?`,
			[]any{*req.RetryOfJobId}, &state, &outcome, &code, &forced, &cancelled, &attempt, &submitted, &released)
		if errors.Is(err, sql.ErrNoRows) {
			return result, ErrRetryNotEligible
		}
		if err != nil {
			return result, err
		}
		if state != "finished" || outcome != "exited" || code == nil || *code == 0 ||
			forced || cancelled != nil || submitted || attempt >= 4 || released == nil {
			return result, ErrRetryNotEligible
		}
		attempt++
	}
	if options.Mode == contract.AdmissionNoWait && options.Admission == nil {
		err := tx.exec("INSERT INTO no_wait_rejections(idempotency_key) VALUES (?)", req.IdempotencyKey)
		result.Busy = err == nil
		return result, err
	}
	id, err := newID()
	if err != nil {
		return result, err
	}
	client := req.Client
	if client == nil {
		client = &pb.ClientInfo{}
	}
	a := req.Attribution
	if a == nil {
		a = &pb.Attribution{}
	}
	extra := a.Extra
	if extra == nil {
		extra = map[string]string{}
	}
	metadata, err := json.Marshal(extra)
	if err != nil || len(metadata) > 16384 {
		return result, errors.Join(errors.New("extra metadata exceeds 16 KiB or cannot be encoded"), err)
	}
	if req.RetryOfJobId != nil {
		if err := tx.exec("UPDATE jobs SET retry_submitted=1 WHERE id=?", *req.RetryOfJobId); err != nil {
			return result, err
		}
	}
	err = tx.exec(`INSERT INTO jobs (id,idempotency_key,request_sha256,command,cwd,weight,state,
		queued_at_ns,service_version,retry_of_job_id,attempt,client_name,client_version,client_os,
		client_os_version,client_architecture,repository,branch,worklog,agent_session,purpose,commit_sha,
		build_target,build_configuration,metadata_json)
		VALUES (?,?,?,?,?,?,'queued',?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, req.IdempotencyKey, hash[:], req.Command, req.Cwd, req.Weight, tx.at,
		options.ServiceVersion, req.RetryOfJobId, attempt, client.Name, client.Version,
		optionalEnum(client.Os.String(), "CLIENT_OS_"), client.OsVersion, optionalEnum(client.Architecture.String(), "CLIENT_ARCHITECTURE_"),
		a.Repository, a.Branch, a.Worklog, a.AgentSession, a.Purpose, a.CommitSha, a.BuildTarget, a.BuildConfiguration, string(metadata))
	if err != nil {
		return result, err
	}
	if options.Mode == contract.AdmissionNoWait {
		if err := tx.reserve(id, req.Runner, options.ServiceRunID, *options.Admission); err != nil {
			return result, err
		}
	} else {
		var sequence int64
		if err := tx.scan("SELECT next_queue_seq FROM broker_state WHERE id=1", nil, &sequence); err != nil {
			return result, err
		}
		if err := tx.exec("UPDATE broker_state SET next_queue_seq=next_queue_seq+1 WHERE id=1"); err != nil {
			return result, err
		}
		start, err := processStart(req.Runner.Process)
		if err != nil {
			return result, err
		}
		if err := tx.exec(`INSERT INTO queue_entries(job_id,queue_seq,runner_incarnation_id,runner_pid,
			runner_start_time_ns,grace_started_at_ns,grace_service_run_id) VALUES (?,?,?,?,?,?,?)`,
			id, sequence, req.Runner.IncarnationId, req.Runner.Process.Pid, start, tx.at, options.ServiceRunID); err != nil {
			return result, err
		}
	}
	result.Job, err = tx.Job(id)
	if err == nil {
		err = tx.jobChanged(id)
	}
	if err == nil {
		err = tx.queueChanged()
	}
	if err == nil && options.Mode == contract.AdmissionNoWait {
		err = tx.statusChanged()
	}
	result.Created = err == nil
	return result, err
}

func optionalEnum(value, prefix string) any {
	if strings.HasSuffix(value, "_UNSPECIFIED") {
		return nil
	}
	return strings.ToLower(strings.TrimPrefix(value, prefix))
}

func (r *Reader) Submission(key string) (*pb.Job, error) {
	var kind, id string
	var count int
	err := r.scan(`SELECT count(*),coalesce(min(kind),''),coalesce(min(id),'') FROM (
		SELECT 'job' AS kind,id FROM jobs WHERE idempotency_key=?
		UNION ALL SELECT 'deleted','' FROM deleted_idempotency_keys WHERE idempotency_key=?
		UNION ALL SELECT 'busy','' FROM no_wait_rejections WHERE idempotency_key=?)`, []any{key, key, key}, &count, &kind, &id)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, ErrNotFound
	}
	if count != 1 {
		err := errors.New("idempotency key has conflicting durable records")
		r.err = errors.Join(r.err, err)
		return nil, err
	}
	switch kind {
	case "deleted":
		return nil, ErrKeyDeleted
	case "busy":
		return nil, ErrAdmissionBusy
	default:
		return r.Job(id)
	}
}

func (r *Reader) Job(id string) (*pb.Job, error) {
	j := &pb.Job{Client: &pb.ClientInfo{}, Attribution: &pb.Attribution{}}
	var state, outcome, metadata string
	var osName, archName, terminationReason *string
	var queued int64
	var admitted, started, finished, cancelled, acknowledged, terminationAt *int64
	o := &pb.Outcome{}
	err := r.scan(`SELECT id,idempotency_key,state,command,cwd,weight,forced,
		service_version,queued_at_ns,admitted_at_ns,started_at_ns,finished_at_ns,
		cancel_requested_at_ns,cancel_reason,cancel_acknowledged_at_ns,cancel_failure_detail,
		broker_termination_at_ns,broker_termination_reason,free_memory_at_admission_bytes,
		admitted_by_backfill,backfill_bypass_count,retry_of_job_id,attempt,
		client_name,client_version,client_os,client_os_version,client_architecture,
		repository,branch,worklog,agent_session,purpose,commit_sha,build_target,build_configuration,metadata_json,
		coalesce(outcome_kind,''),exit_code,signal_number,signal_name,outcome_detail FROM jobs WHERE id=?`,
		[]any{id}, &j.JobId, &j.IdempotencyKey, &state, &j.Command, &j.Cwd, &j.Weight, &j.Forced,
		&j.ServiceVersion, &queued, &admitted, &started, &finished,
		&cancelled, &j.CancelReason, &acknowledged, &j.CancelFailureDetail,
		&terminationAt, &terminationReason, &j.FreeMemoryAtAdmissionBytes,
		&j.AdmittedByBackfill, &j.BackfillBypassCount, &j.RetryOfJobId, &j.Attempt,
		&j.Client.Name, &j.Client.Version, &osName, &j.Client.OsVersion, &archName,
		&j.Attribution.Repository, &j.Attribution.Branch, &j.Attribution.Worklog, &j.Attribution.AgentSession,
		&j.Attribution.Purpose, &j.Attribution.CommitSha, &j.Attribution.BuildTarget, &j.Attribution.BuildConfiguration, &metadata,
		&outcome, &o.ExitCode, &o.SignalNumber, &o.SignalName, &o.Detail)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	j.QueuedAt = contract.Timestamp(queued)
	j.AdmittedAt, j.StartedAt, j.FinishedAt = optionalTimestamp(admitted), optionalTimestamp(started), optionalTimestamp(finished)
	j.CancelRequestedAt, j.CancelAcknowledgedAt = optionalTimestamp(cancelled), optionalTimestamp(acknowledged)
	j.State = pb.JobState(pb.JobState_value["JOB_STATE_"+strings.ToUpper(state)])
	if osName != nil {
		j.Client.Os = pb.ClientOS(pb.ClientOS_value["CLIENT_OS_"+strings.ToUpper(*osName)])
	}
	if archName != nil {
		j.Client.Architecture = pb.ClientArchitecture(pb.ClientArchitecture_value["CLIENT_ARCHITECTURE_"+strings.ToUpper(*archName)])
	}
	if err := json.Unmarshal([]byte(metadata), &j.Attribution.Extra); err != nil {
		r.err = errors.Join(r.err, err)
		return nil, err
	}
	if outcome != "" {
		o.Kind = pb.OutcomeKind(pb.OutcomeKind_value["OUTCOME_KIND_"+strings.ToUpper(outcome)])
		j.Outcome = o
	}
	if terminationAt != nil {
		j.BrokerTermination = &pb.BrokerTermination{StartedAt: contract.Timestamp(*terminationAt),
			Reason: pb.BrokerTermination_Reason(pb.BrokerTermination_Reason_value["REASON_"+strings.ToUpper(*terminationReason)])}
	}
	if j.State == pb.JobState_JOB_STATE_QUEUED {
		var pos uint32
		err := r.scan(`SELECT (SELECT count(*) FROM queue_entries p WHERE p.queue_seq<q.queue_seq),
			runner_connected FROM queue_entries q WHERE job_id=?`, []any{id}, &pos, &j.RunnerConnected)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			j.QueuePosition = &pos
		}
	}
	g := &pb.Grant{Runner: &pb.RunnerIncarnation{Process: &pb.ProcessIdentity{}}}
	var granted, runnerStart int64
	err = r.scan(`SELECT grant_id,weight,forced,granted_at_ns,runner_incarnation_id,
		runner_pid,runner_start_time_ns FROM reservations WHERE job_id=? AND released_at_ns IS NULL`,
		[]any{id}, &g.GrantId, &g.Weight, &g.Forced, &granted, &g.Runner.IncarnationId, &g.Runner.Process.Pid, &runnerStart)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		g.GrantedAt = contract.Timestamp(granted)
		g.Runner.Process.StartTime = contract.Timestamp(runnerStart)
		j.Grant = g
	}
	p := &pb.ProcessIdentity{}
	var processStartNs int64
	err = r.scan("SELECT pid,start_time_ns FROM job_processes WHERE job_id=? AND role='command'", []any{id}, &p.Pid, &processStartNs)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		p.StartTime = contract.Timestamp(processStartNs)
		j.Process = p
	}
	summary := &pb.ResourceSummary{}
	var completeness string
	var elapsedMs, cpuMs *int64
	err = r.scan(`SELECT elapsed_ms,cpu_time_ms,peak_cpu_percent,peak_combined_rss_bytes,
		sample_count,measurement_completeness,incompleteness_reason FROM job_resource_summaries WHERE job_id=?`,
		[]any{id}, &elapsedMs, &cpuMs, &summary.PeakCpuPercent, &summary.PeakCombinedRssBytes,
		&summary.SampleCount, &completeness, &summary.IncompletenessReason)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		if summary.Elapsed, err = r.storedDuration(elapsedMs); err != nil {
			return nil, err
		}
		if summary.CpuTime, err = r.storedDuration(cpuMs); err != nil {
			return nil, err
		}
		summary.Completeness = pb.MeasurementCompleteness(pb.MeasurementCompleteness_value["MEASUREMENT_COMPLETENESS_"+strings.ToUpper(completeness)])
		j.Resources = summary
	}
	return j, nil
}

func (r *Reader) ids(statement string, args ...any) ([]string, error) {
	rows, err := r.q.QueryContext(r.ctx, statement, args...)
	if err != nil {
		r.err = errors.Join(r.err, err)
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		r.err = errors.Join(r.err, err)
	}
	return ids, err
}

func (r *Reader) Queue() (*pb.Queue, error) {
	ids, err := r.ids("SELECT job_id FROM queue_entries ORDER BY queue_seq")
	if err != nil {
		return nil, err
	}
	queue := &pb.Queue{}
	for _, id := range ids {
		j, err := r.Job(id)
		if err != nil {
			return nil, err
		}
		queue.Jobs = append(queue.Jobs, j)
	}
	return queue, nil
}

func (r *Reader) Status() (*pb.BrokerStatus, error) {
	s := &pb.BrokerStatus{}
	var mode string
	var pausedAt *int64
	err := r.scan(`SELECT queue_mode,paused_at_ns,
		(SELECT coalesce(sum(weight),0) FROM reservations WHERE released_at_ns IS NULL),
		(SELECT count(*) FROM queue_entries),
		(SELECT count(*) FROM jobs WHERE state IN ('running','finishing')) FROM broker_state WHERE id=1`,
		nil, &mode, &pausedAt, &s.Occupied, &s.QueuedJobs, &s.RunningJobs)
	if err != nil {
		return s, err
	}
	s.PausedAt = optionalTimestamp(pausedAt)
	switch mode {
	case "enabled":
		s.QueueMode = pb.QueueMode_QUEUE_MODE_ENABLED
	case "paused":
		s.QueueMode = pb.QueueMode_QUEUE_MODE_PAUSED
	default:
		return s, fmt.Errorf("invalid queue mode %q", mode)
	}
	// Deprecated, but still set for older clients.
	s.Paused = s.QueueMode == pb.QueueMode_QUEUE_MODE_PAUSED
	return s, nil
}
