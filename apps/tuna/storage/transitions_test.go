package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/boumenot/heavy-lock/apps/tuna/contract"
	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"google.golang.org/protobuf/proto"
)

type fixture struct {
	t     *testing.T
	ctx   context.Context
	store *Store
	path  string
	run   int64
}

func setup(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, ctx: context.Background(), path: filepath.Join(t.TempDir(), "state.db")}
	var err error
	f.store, err = Open(f.ctx, f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.store.Close(); err != nil {
			t.Error(err)
		}
	})
	f.write(func(tx *Tx) error { var err error; f.run, err = tx.StartService("test", 1); return err })
	return f
}

func (f *fixture) write(fn func(*Tx) error) []*pb.BrokerEvent {
	f.t.Helper()
	events, err := f.store.Write(f.ctx, fn)
	if err != nil {
		f.t.Fatal(err)
	}
	return events
}

func (f *fixture) request() *pb.SubmitJobRequest {
	f.t.Helper()
	key, err := newID()
	if err != nil {
		f.t.Fatal(err)
	}
	incarnation, err := newID()
	if err != nil {
		f.t.Fatal(err)
	}
	return &pb.SubmitJobRequest{IdempotencyKey: key, Command: "example", Weight: 1,
		Runner: &pb.RunnerIncarnation{IncarnationId: incarnation, Process: &pb.ProcessIdentity{Pid: 1, StartTime: contract.Timestamp(1)}}}
}

func (f *fixture) submit(req *pb.SubmitJobRequest, mode contract.AdmissionMode, admission *Admission) SubmissionResult {
	f.t.Helper()
	var result SubmissionResult
	f.write(func(tx *Tx) error {
		var err error
		result, err = tx.Submit(req, Submission{Mode: mode, ServiceVersion: "test", ServiceRunID: f.run, Admission: admission})
		return err
	})
	return result
}

func (f *fixture) job(id string) *pb.Job {
	f.t.Helper()
	var j *pb.Job
	err := f.store.View(f.ctx, func(r *Reader) error { var err error; j, err = r.Job(id); return err })
	if err != nil {
		f.t.Fatal(err)
	}
	return j
}

func (f *fixture) expect(want error, fn func(*Tx) error) {
	f.t.Helper()
	events, err := f.store.Write(f.ctx, fn)
	if !errors.Is(err, want) || len(events) != 0 {
		f.t.Fatalf("wanted %v, got events=%v error=%v", want, events, err)
	}
}

func (f *fixture) finish(j *pb.Job, req *pb.SubmitJobRequest, code int64) {
	f.t.Helper()
	root := &pb.ProcessIdentity{Pid: 2, StartTime: contract.Timestamp(3)}
	f.write(func(tx *Tx) error {
		if _, err := tx.Claim(j.JobId, j.Grant.GrantId, req.Runner); err != nil {
			return err
		}
		if err := tx.ReportFinished(j.JobId, j.Grant.GrantId, req.Runner, root, &pb.Outcome{Kind: pb.OutcomeKind_OUTCOME_KIND_EXITED, ExitCode: &code}); err != nil {
			return err
		}
		if err := tx.Seal(j.JobId, j.Grant.GrantId, req.Runner, false); err != nil {
			return err
		}
		if err := tx.VerifyProcessExited(j.JobId, root); err != nil {
			return err
		}
		return tx.Release(j.JobId, "processes_exited")
	})
}

func TestSubmissionMetadataAndResourceRoundTrip(t *testing.T) {
	f := setup(t)
	req := f.request()
	req.Cwd = proto.String("/example")
	req.Client = &pb.ClientInfo{Name: proto.String("runner"), Version: proto.String("test"),
		Os: pb.ClientOS_CLIENT_OS_MACOS, OsVersion: proto.String("26.1"), Architecture: pb.ClientArchitecture_CLIENT_ARCHITECTURE_ARM64}
	req.Attribution = &pb.Attribution{Repository: proto.String("owner/repo"), Branch: proto.String("feature"),
		Worklog: proto.String("owner/repo#1"), AgentSession: proto.String("session"), Purpose: proto.String("build"),
		CommitSha: proto.String("0123456"), BuildTarget: proto.String("test"), BuildConfiguration: proto.String("debug"),
		Extra: map[string]string{"tool.example": "value"}}
	j := f.submit(req, contract.AdmissionQueued, nil).Job
	if !proto.Equal(j.Client, req.Client) || !proto.Equal(j.Attribution, req.Attribution) || j.GetCwd() != req.GetCwd() {
		t.Fatalf("submission metadata was changed or lost: %v", j)
	}
	summary := &pb.ResourceSummary{Elapsed: contract.MillisecondDuration(2), CpuTime: contract.MillisecondDuration(1), PeakCpuPercent: proto.Float64(125.5),
		PeakCombinedRssBytes: proto.Int64(100), SampleCount: 1, Completeness: pb.MeasurementCompleteness_MEASUREMENT_COMPLETENESS_PARTIAL,
		IncompletenessReason: proto.String("sampling gap")}
	f.write(func(tx *Tx) error { return tx.SaveResources(j.JobId, summary) })
	if !proto.Equal(f.job(j.JobId).Resources, summary) {
		t.Fatal("resource summary was changed or lost")
	}
}

func TestNoWaitBusyAndDeletedKeysSurviveReopen(t *testing.T) {
	f := setup(t)
	busy := f.request()
	if result := f.submit(busy, contract.AdmissionNoWait, nil); !result.Busy || result.Created || result.Job != nil {
		t.Fatalf("busy result: %+v", result)
	}
	busy.Command = "changed"
	if result := f.submit(busy, contract.AdmissionQueued, nil); !result.Busy {
		t.Fatal("busy key accepted")
	}
	req := f.request()
	j := f.submit(req, contract.AdmissionNoWait, &Admission{}).Job
	f.finish(j, req, 1)
	f.write(func(tx *Tx) error { return tx.DeleteJob(j.JobId) })
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.store, err = Open(f.ctx, f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.expect(ErrKeyDeleted, func(tx *Tx) error { _, err := tx.Submit(req, Submission{Mode: contract.AdmissionNoWait}); return err })
	err = f.store.View(f.ctx, func(r *Reader) error { _, err := r.Submission(busy.IdempotencyKey); return err })
	if !errors.Is(err, ErrAdmissionBusy) {
		t.Fatalf("busy after reopen: %v", err)
	}
	f.write(func(tx *Tx) error {
		var jobs, queues, keys int
		if err := tx.scan("SELECT count(*) FROM jobs", nil, &jobs); err != nil {
			return err
		}
		if err := tx.scan("SELECT count(*) FROM queue_entries", nil, &queues); err != nil {
			return err
		}
		if err := tx.scan("SELECT count(*) FROM deleted_idempotency_keys", nil, &keys); err != nil {
			return err
		}
		if jobs != 0 || queues != 0 || keys != 1 {
			t.Fatalf("jobs %d queues %d tombstones %d", jobs, queues, keys)
		}
		return nil
	})
}

func TestRetryLineageEligibilityAndBusyDoesNotConsumeParent(t *testing.T) {
	f := setup(t)
	req := f.request()
	parent := f.submit(req, contract.AdmissionNoWait, &Admission{}).Job
	f.finish(parent, req, 1)
	retry := f.request()
	retry.RetryOfJobId = &parent.JobId
	if result := f.submit(retry, contract.AdmissionNoWait, nil); !result.Busy {
		t.Fatal("retry not busy")
	}
	retry = f.request()
	retry.RetryOfJobId = &parent.JobId
	child := f.submit(retry, contract.AdmissionNoWait, &Admission{}).Job
	if child.Attempt != 2 || child.GetRetryOfJobId() != parent.JobId {
		t.Fatalf("lineage: %v", child)
	}
	f.write(func(tx *Tx) error { return tx.DeleteJob(parent.JobId) })
	before := f.job(child.JobId)
	duplicate := f.submit(retry, contract.AdmissionNoWait, &Admission{}).Job
	if !proto.Equal(before, duplicate) {
		t.Fatal("deletion changed surviving retry")
	}
	finish := func(tx *Tx) error {
		fresh := f.request()
		fresh.RetryOfJobId = &parent.JobId
		_, err := tx.Submit(fresh, Submission{Mode: contract.AdmissionQueued, ServiceRunID: f.run, ServiceVersion: "test"})
		return err
	}
	f.expect(ErrRetryNotEligible, finish)
	for attempt := uint32(3); attempt <= 4; attempt++ {
		f.finish(child, retry, 1)
		next := f.request()
		next.RetryOfJobId = &child.JobId
		newChild := f.submit(next, contract.AdmissionQueued, nil).Job
		if newChild.Attempt != attempt {
			t.Fatalf("attempt %d", newChild.Attempt)
		}
		f.write(func(tx *Tx) error {
			if err := tx.DeleteJob(child.JobId); err != nil {
				return err
			}
			if err := tx.SetQueueConnection(newChild.JobId, next.Runner, true, f.run); err != nil {
				return err
			}
			return tx.Reserve(newChild.JobId, f.run, Admission{})
		})
		child = f.job(newChild.JobId)
		retry = next
	}
	f.finish(child, retry, 1)
	fresh := f.request()
	fresh.RetryOfJobId = &child.JobId
	f.expect(ErrRetryNotEligible, func(tx *Tx) error {
		_, err := tx.Submit(fresh, Submission{Mode: contract.AdmissionQueued, ServiceRunID: f.run, ServiceVersion: "test"})
		return err
	})
}

func TestRetryRejectsIneligibleOutcomesAndConsumedParent(t *testing.T) {
	for _, scenario := range []string{"success", "forced", "cancelled", "held", "already retried", "retry deleted"} {
		t.Run(scenario, func(t *testing.T) {
			f := setup(t)
			req := f.request()
			var j *pb.Job
			if scenario == "forced" {
				j = f.submit(req, contract.AdmissionQueued, nil).Job
				f.write(func(tx *Tx) error {
					if err := tx.SetQueueConnection(j.JobId, req.Runner, true, f.run); err != nil {
						return err
					}
					return tx.Reserve(j.JobId, f.run, Admission{Forced: true})
				})
				j = f.job(j.JobId)
			} else {
				j = f.submit(req, contract.AdmissionNoWait, &Admission{}).Job
			}
			if scenario == "cancelled" {
				f.write(func(tx *Tx) error { return tx.Cancel(j.JobId, nil) })
			} else if scenario != "held" {
				code := int64(1)
				if scenario == "success" {
					code = 0
				}
				f.finish(j, req, code)
			}
			if scenario == "already retried" || scenario == "retry deleted" {
				r := f.request()
				r.RetryOfJobId = &j.JobId
				child := f.submit(r, contract.AdmissionNoWait, &Admission{}).Job
				if scenario == "retry deleted" {
					f.finish(child, r, 1)
					f.write(func(tx *Tx) error { return tx.DeleteJob(child.JobId) })
				}
			}
			retry := f.request()
			retry.RetryOfJobId = &j.JobId
			f.expect(ErrRetryNotEligible, func(tx *Tx) error {
				_, err := tx.Submit(retry, Submission{Mode: contract.AdmissionQueued, ServiceVersion: "test", ServiceRunID: f.run})
				return err
			})
		})
	}
}

func TestClaimSealOutcomeAndReleaseEvidence(t *testing.T) {
	f := setup(t)
	req := f.request()
	j := f.submit(req, contract.AdmissionNoWait, &Admission{}).Job
	wrong := f.request().Runner
	f.expect(ErrIdentity, func(tx *Tx) error { _, err := tx.Claim(j.JobId, j.Grant.GrantId, wrong); return err })
	f.write(func(tx *Tx) error {
		already, err := tx.Claim(j.JobId, j.Grant.GrantId, req.Runner)
		if already {
			t.Fatal("first claim marked duplicate")
		}
		return err
	})
	if events := f.write(func(tx *Tx) error {
		already, err := tx.Claim(j.JobId, j.Grant.GrantId, req.Runner)
		if !already {
			t.Fatal("second claim not duplicate")
		}
		return err
	}); len(events) != 0 {
		t.Fatal("duplicate claim emitted event")
	}
	root := &pb.ProcessIdentity{Pid: 2, StartTime: contract.Timestamp(3)}
	code := int64(1)
	outcome := &pb.Outcome{Kind: pb.OutcomeKind_OUTCOME_KIND_EXITED, ExitCode: &code}
	f.write(func(tx *Tx) error { return tx.ReportFinished(j.JobId, j.Grant.GrantId, req.Runner, root, outcome) })
	if f.job(j.JobId).State != pb.JobState_JOB_STATE_FINISHING {
		t.Fatal("outcome released capacity")
	}
	f.expect(ErrUnreconciled, func(tx *Tx) error { return tx.Release(j.JobId, "processes_exited") })
	f.write(func(tx *Tx) error { return tx.Seal(j.JobId, j.Grant.GrantId, req.Runner, false) })
	f.expect(ErrIdentity, func(tx *Tx) error { return tx.Seal(j.JobId, j.Grant.GrantId, req.Runner, true) })
	f.expect(ErrUnreconciled, func(tx *Tx) error { return tx.Release(j.JobId, "processes_exited") })
	f.expect(ErrIdentity, func(tx *Tx) error {
		return tx.VerifyProcessExited(j.JobId, &pb.ProcessIdentity{Pid: 2, StartTime: contract.Timestamp(4)})
	})
	f.write(func(tx *Tx) error {
		if err := tx.TrackProcess(j.JobId, &pb.ProcessIdentity{Pid: 3, StartTime: contract.Timestamp(4)}, "descendant"); err != nil {
			return err
		}
		return tx.VerifyProcessExited(j.JobId, root)
	})
	f.expect(ErrUnreconciled, func(tx *Tx) error { return tx.Release(j.JobId, "processes_exited") })
	f.write(func(tx *Tx) error {
		if err := tx.VerifyProcessExited(j.JobId, &pb.ProcessIdentity{Pid: 3, StartTime: contract.Timestamp(4)}); err != nil {
			return err
		}
		return tx.Release(j.JobId, "processes_exited")
	})
	if events := f.write(func(tx *Tx) error { return tx.ReportFinished(j.JobId, j.Grant.GrantId, req.Runner, root, outcome) }); len(events) != 0 {
		t.Fatal("repeat finish changed history")
	}
	code = 2
	f.expect(ErrIdentity, func(tx *Tx) error { return tx.ReportFinished(j.JobId, j.Grant.GrantId, req.Runner, root, outcome) })
}

func TestCancellationAndLaunchFailureNeverReleaseClaimedWork(t *testing.T) {
	f := setup(t)
	req := f.request()
	j := f.submit(req, contract.AdmissionNoWait, &Admission{}).Job
	f.write(func(tx *Tx) error { _, err := tx.Claim(j.JobId, j.Grant.GrantId, req.Runner); return err })
	f.write(func(tx *Tx) error { return tx.Cancel(j.JobId, proto.String("first")) })
	at := f.job(j.JobId).GetCancelRequestedAt()
	if events := f.write(func(tx *Tx) error { return tx.Cancel(j.JobId, proto.String("other")) }); len(events) != 0 {
		t.Fatal("cancel repeat changed")
	}
	f.write(func(tx *Tx) error { return tx.ReportCancelFailed(j.JobId, j.Grant.GrantId, req.Runner, "denied") })
	f.expect(ErrIdentity, func(tx *Tx) error { return tx.ReportCancelFailed(j.JobId, j.Grant.GrantId, req.Runner, "different") })
	f.write(func(tx *Tx) error {
		return tx.RecordTermination(j.JobId, pb.BrokerTermination_REASON_CANCEL_RUNNER_FAILED)
	})
	termAt := f.job(j.JobId).GetBrokerTermination().GetStartedAt()
	if events := f.write(func(tx *Tx) error {
		return tx.RecordTermination(j.JobId, pb.BrokerTermination_REASON_CANCEL_RUNNER_FAILED)
	}); len(events) != 0 {
		t.Fatal("repeat termination changed history")
	}
	f.expect(ErrIdentity, func(tx *Tx) error { return tx.RecordTermination(j.JobId, pb.BrokerTermination_REASON_RUNNER_DEAD) })
	if got := f.job(j.JobId).GetBrokerTermination(); !proto.Equal(got.GetStartedAt(), termAt) ||
		got.GetReason() != pb.BrokerTermination_REASON_CANCEL_RUNNER_FAILED {
		t.Fatalf("conflicting termination overwrote sticky record: %v", got)
	}
	f.write(func(tx *Tx) error {
		return tx.ReportLaunchFailed(j.JobId, j.Grant.GrantId, req.Runner, "no spawn", true)
	})
	j = f.job(j.JobId)
	if j.Grant == nil || j.FinishedAt != nil || j.Outcome.Kind != pb.OutcomeKind_OUTCOME_KIND_CANCELLED_BEFORE_START ||
		!proto.Equal(j.GetCancelRequestedAt(), at) || j.BrokerTermination.Reason != pb.BrokerTermination_REASON_CANCEL_RUNNER_FAILED {
		t.Fatalf("cancel evidence lost: %v", j)
	}
	f.expect(ErrUnreconciled, func(tx *Tx) error { return tx.Release(j.JobId, "cancelled_before_start") })
	f.write(func(tx *Tx) error {
		if err := tx.Seal(j.JobId, j.Grant.GrantId, req.Runner, true); err != nil {
			return err
		}
		return tx.Release(j.JobId, "cancelled_before_start")
	})
}

func TestLaunchFailureNeedsSealAndRetainsDefiniteOutcome(t *testing.T) {
	f := setup(t)
	req := f.request()
	j := f.submit(req, contract.AdmissionNoWait, &Admission{}).Job
	f.write(func(tx *Tx) error {
		if _, err := tx.Claim(j.JobId, j.Grant.GrantId, req.Runner); err != nil {
			return err
		}
		return tx.ReportLaunchFailed(j.JobId, j.Grant.GrantId, req.Runner, "spawn failed", false)
	})
	reported := f.job(j.JobId)
	if reported.State != pb.JobState_JOB_STATE_RESERVED || reported.Grant == nil ||
		reported.Outcome.Kind != pb.OutcomeKind_OUTCOME_KIND_LAUNCH_FAILED {
		t.Fatalf("report: %v", reported)
	}
	f.expect(ErrIdentity, func(tx *Tx) error {
		return tx.ReportStarted(j.JobId, j.Grant.GrantId, req.Runner, &pb.ProcessIdentity{Pid: 2, StartTime: contract.Timestamp(3)})
	})
	f.expect(ErrIdentity, func(tx *Tx) error {
		return tx.ReportLaunchFailed(j.JobId, j.Grant.GrantId, req.Runner, "different", false)
	})
	f.expect(ErrUnreconciled, func(tx *Tx) error { return tx.Release(j.JobId, "launch_failed") })
	f.write(func(tx *Tx) error {
		if err := tx.Seal(j.JobId, j.Grant.GrantId, req.Runner, true); err != nil {
			return err
		}
		return tx.Release(j.JobId, "launch_failed")
	})
	result := f.job(j.JobId)
	if result.State != pb.JobState_JOB_STATE_FINISHED || result.Grant != nil || result.Outcome.GetDetail() != "spawn failed" {
		t.Fatalf("release: %v", result)
	}
}

func TestLateOutcomeReplacesUnknownWithoutChangingRelease(t *testing.T) {
	f := setup(t)
	req := f.request()
	j := f.submit(req, contract.AdmissionNoWait, &Admission{}).Job
	var releaseAt int64
	f.write(func(tx *Tx) error {
		if _, err := tx.Claim(j.JobId, j.Grant.GrantId, req.Runner); err != nil {
			return err
		}
		if err := tx.MarkUnknown(j.JobId, proto.String("recovery")); err != nil {
			return err
		}
		if err := tx.FenceRunnerTerminated(j.JobId, req.Runner); err != nil {
			return err
		}
		if err := tx.Release(j.JobId, "recovery_verified_gone"); err != nil {
			return err
		}
		res, err := tx.Reservation(j.JobId)
		if err == nil {
			releaseAt = *res.ReleasedAt
		}
		return err
	})
	f.write(func(tx *Tx) error {
		if err := tx.ReportFinished(j.JobId, j.Grant.GrantId, req.Runner,
			&pb.ProcessIdentity{Pid: 2, StartTime: contract.Timestamp(3)},
			&pb.Outcome{Kind: pb.OutcomeKind_OUTCOME_KIND_EXITED, ExitCode: proto.Int64(1)}); err != nil {
			return err
		}
		res, err := tx.Reservation(j.JobId)
		if err == nil && (*res.ReleasedAt != releaseAt || *res.ReleaseReason != "recovery_verified_gone") {
			t.Fatal("late report changed release evidence")
		}
		return err
	})
	result := f.job(j.JobId)
	if result.State != pb.JobState_JOB_STATE_FINISHED || result.Outcome.Kind != pb.OutcomeKind_OUTCOME_KIND_EXITED {
		t.Fatalf("late report: %v", result)
	}
	f.expect(ErrUnreconciled, func(tx *Tx) error { return tx.DeleteJob(j.JobId) })
}

func TestUnclaimedCancelFencesAndQueuedCancelIsTerminal(t *testing.T) {
	f := setup(t)
	for _, mode := range []contract.AdmissionMode{contract.AdmissionQueued, contract.AdmissionNoWait} {
		req := f.request()
		var admission *Admission
		if mode == contract.AdmissionNoWait {
			admission = &Admission{}
		}
		j := f.submit(req, mode, admission).Job
		f.write(func(tx *Tx) error { return tx.Cancel(j.JobId, nil) })
		result := f.job(j.JobId)
		if result.State != pb.JobState_JOB_STATE_FINISHED || result.Grant != nil || result.Outcome.Kind != pb.OutcomeKind_OUTCOME_KIND_CANCELLED_BEFORE_START {
			t.Fatalf("cancel: %v", result)
		}
		if mode == contract.AdmissionNoWait {
			f.expect(ErrTransition, func(tx *Tx) error { _, err := tx.Claim(j.JobId, j.Grant.GrantId, req.Runner); return err })
		}
	}
}

func TestQueuePauseReorderGraceAndForcedOccupancy(t *testing.T) {
	f := setup(t)
	a, b := f.request(), f.request()
	first := f.submit(a, contract.AdmissionQueued, nil).Job
	second := f.submit(b, contract.AdmissionQueued, nil).Job
	f.expect(ErrTransition, func(tx *Tx) error { return tx.Reserve(first.JobId, f.run, Admission{}) })
	f.write(func(tx *Tx) error {
		if err := tx.CheckpointGrace(first.JobId, 100, f.run); err != nil {
			return err
		}
		// A valid preexisting negative move-front sequence must not collide
		// with the renumbering transaction's temporary values.
		if err := tx.exec("UPDATE queue_entries SET queue_seq=-1 WHERE job_id=?", first.JobId); err != nil {
			return err
		}
		if err := tx.MoveBefore(second.JobId, first.JobId); err != nil {
			return err
		}
		if err := tx.MoveBefore(first.JobId, ""); err != nil {
			return err
		}
		if err := tx.SetQueueConnection(first.JobId, a.Runner, true, f.run); err != nil {
			return err
		}
		if err := tx.SetQueueMode(pb.QueueMode_QUEUE_MODE_PAUSED); err != nil {
			return err
		}
		if err := tx.ReadyService(f.run); err != nil {
			return err
		}
		return tx.Reserve(first.JobId, f.run, Admission{Forced: true})
	})
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.store, err = Open(f.ctx, f.path)
	if err != nil {
		t.Fatal(err)
	}
	err = f.store.View(f.ctx, func(r *Reader) error {
		status, err := r.Status()
		if err != nil {
			return err
		}
		if status.QueueMode != pb.QueueMode_QUEUE_MODE_PAUSED || status.Occupied != 1 || status.QueuedJobs != 1 {
			t.Fatalf("durable status: %v", status)
		}
		runs, err := r.ServiceRuns()
		if err != nil {
			return err
		}
		if len(runs) != 1 || runs[0].ReadyAt == nil || runs[0].CleanShutdown {
			t.Fatalf("run: %+v", runs)
		}
		queue, err := r.Queue()
		if err != nil {
			return err
		}
		if len(queue.Jobs) != 1 || queue.Jobs[0].JobId != second.JobId {
			t.Fatalf("queue %v", queue)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEventsPruningDeletionAndBulkHistoryKeepLiveRetry(t *testing.T) {
	f := setup(t)
	req := f.request()
	parent := f.submit(req, contract.AdmissionNoWait, &Admission{}).Job
	f.finish(parent, req, 1)
	retry := f.request()
	retry.RetryOfJobId = &parent.JobId
	child := f.submit(retry, contract.AdmissionNoWait, &Admission{}).Job
	f.write(func(tx *Tx) error {
		if err := tx.SaveResources(parent.JobId, &pb.ResourceSummary{Completeness: pb.MeasurementCompleteness_MEASUREMENT_COMPLETENESS_PARTIAL, SampleCount: 1}); err != nil {
			return err
		}
		deleted, skipped, err := tx.DeleteHistory(time.Now().UnixNano() + 1)
		if deleted != 1 || skipped != 1 {
			t.Fatalf("bulk: deleted %d skipped %d", deleted, skipped)
		}
		return err
	})
	if !proto.Equal(child, f.job(child.JobId)) {
		t.Fatal("bulk deleted changed active retry")
	}
	f.write(func(tx *Tx) error {
		var floor, last int64
		if err := tx.scan("SELECT event_floor_seq,last_event_seq FROM broker_state", nil, &floor, &last); err != nil {
			return err
		}
		if floor < 1 || last <= floor {
			t.Fatalf("floor=%d last=%d", floor, last)
		}
		if _, err := tx.Events(floor-1, 100); !errors.Is(err, ErrEventsExpired) {
			t.Fatalf("gap replay: %v", err)
		}
		if _, err := tx.Events(last+1, 100); !errors.Is(err, ErrEventsExpired) {
			t.Fatalf("future replay: %v", err)
		}
		events, err := tx.Events(floor, 100)
		if err != nil {
			return err
		}
		for _, event := range events {
			if event.GetJobChanged().GetJobId() == parent.JobId || event.GetQueueChanged() != nil {
				t.Fatal("deleted payload retained")
			}
		}
		if err := tx.PruneEvents(0, 1); err != nil {
			return err
		}
		var count int
		if err := tx.scan("SELECT count(*) FROM events", nil, &count); err != nil {
			return err
		}
		if count != 1 {
			t.Fatalf("prune count %d", count)
		}
		j, err := tx.Job(child.JobId)
		if err != nil {
			return err
		}
		if j.Grant == nil {
			t.Fatal("pruning removed recovery state")
		}
		return nil
	})
}
