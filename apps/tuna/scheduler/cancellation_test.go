package scheduler

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/boumenot/heavy-lock/apps/tuna/contract"
	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"github.com/boumenot/heavy-lock/apps/tuna/storage"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestCancelAcknowledgmentDeadlineDoesNotReset(t *testing.T) {
	probe := &fakeProbe{}
	h, _ := launchHarness(t, 2, probe)
	l := h.granted(2)
	h.start1(&l, 4242)
	if _, err := h.s.Cancel(h.ctx, l.id, nil); err != nil {
		t.Fatal(err)
	}
	h.clock.advance(4 * time.Second)
	if _, err := h.s.Cancel(h.ctx, l.id, nil); err != nil {
		t.Fatal(err)
	}
	h.watch(l.req, l.id)
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if j := h.job(l.id); j.BrokerTermination != nil {
		t.Fatalf("takeover before the acknowledgment deadline: %v", j.BrokerTermination)
	}
	h.clock.advance(time.Second)
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	j := h.job(l.id)
	if j.BrokerTermination.GetReason() != pb.BrokerTermination_REASON_CANCEL_ACK_DEADLINE {
		t.Fatalf("takeover after acknowledgment deadline = %v", j.BrokerTermination)
	}
	if j.Outcome.GetKind() != pb.OutcomeKind_OUTCOME_KIND_UNKNOWN ||
		!strings.Contains(j.Outcome.GetDetail(), "fingerprint-checked") {
		t.Fatalf("takeover termination was not exposed: %v", j.Outcome)
	}
	if h.snapshot().Status.Occupied != 2 || j.State == pb.JobState_JOB_STATE_FINISHED {
		t.Fatalf("deadline released the reservation: %v", j)
	}
	if _, _, err := h.s.AcknowledgeCancel(h.ctx, l.id, l.grant, l.req.Runner); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.s.ReportCancelFailed(h.ctx, l.id, l.grant, l.req.Runner, "late failure"); err != nil {
		t.Fatal(err)
	}
	probe.exit(l.req.Runner.Process.Pid)
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.job(l.id).BrokerTermination.GetReason(); got != pb.BrokerTermination_REASON_CANCEL_ACK_DEADLINE {
		t.Fatalf("late evidence replaced takeover reason: %v", got)
	}
	probe.exit(l.proc.Pid)
	j, _, err := h.s.ReportFinished(h.ctx, l.id, l.grant, l.req.Runner, l.proc, exited(1))
	if err != nil || j.State != pb.JobState_JOB_STATE_FINISHED ||
		j.BrokerTermination.GetReason() != pb.BrokerTermination_REASON_CANCEL_ACK_DEADLINE {
		t.Fatalf("late outcome lost takeover history: job=%v err=%v", j, err)
	}
}

func TestCancelCompletionDeadlineIncludesKnownDescendants(t *testing.T) {
	probe := &fakeProbe{}
	h, _ := launchHarness(t, 2, probe)
	l := h.granted(2)
	h.start1(&l, 4242)
	descendant := &pb.ProcessIdentity{Pid: 4243, StartTime: contract.Timestamp(5243)}
	if _, err := h.store.Write(h.ctx, func(tx *storage.Tx) error {
		return tx.TrackProcess(l.id, descendant, "descendant")
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Cancel(h.ctx, l.id, nil); err != nil {
		t.Fatal(err)
	}
	h.clock.advance(4 * time.Second)
	if _, _, err := h.s.AcknowledgeCancel(h.ctx, l.id, l.grant, l.req.Runner); err != nil {
		t.Fatal(err)
	}
	probe.exit(l.proc.Pid)
	if _, _, err := h.s.ReportFinished(h.ctx, l.id, l.grant, l.req.Runner, l.proc, exited(1)); err != nil {
		t.Fatal(err)
	}
	h.clock.advance(29 * time.Second)
	if _, _, err := h.s.AcknowledgeCancel(h.ctx, l.id, l.grant, l.req.Runner); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if j := h.job(l.id); j.BrokerTermination != nil || j.State == pb.JobState_JOB_STATE_FINISHED {
		t.Fatalf("acknowledgment or root outcome ended cleanup: %v", j)
	}
	h.clock.advance(time.Second)
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if j := h.job(l.id); j.BrokerTermination.GetReason() != pb.BrokerTermination_REASON_CANCEL_COMPLETION_DEADLINE ||
		j.State == pb.JobState_JOB_STATE_FINISHED {
		t.Fatalf("completion deadline with live descendant: %v", j)
	}
}

func TestCancelTakeoverPriorityAndCompletedCleanup(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		dead     bool
		finished bool
		want     pb.BrokerTermination_Reason
	}{
		{name: "runner failure", want: pb.BrokerTermination_REASON_CANCEL_RUNNER_FAILED},
		{name: "runner death takes priority", dead: true, want: pb.BrokerTermination_REASON_RUNNER_DEAD},
		{name: "completed cleanup needs no takeover", finished: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			probe := &fakeProbe{}
			h, _ := launchHarness(t, 2, probe)
			l := h.granted(2)
			h.start1(&l, 4242)
			if _, err := h.s.Cancel(h.ctx, l.id, nil); err != nil {
				t.Fatal(err)
			}
			if scenario.finished {
				probe.exit(l.proc.Pid)
				if _, _, err := h.s.ReportFinished(h.ctx, l.id, l.grant, l.req.Runner, l.proc, exited(0)); err != nil {
					t.Fatal(err)
				}
			} else if _, _, err := h.s.ReportCancelFailed(h.ctx, l.id, l.grant, l.req.Runner, "stop failed"); err != nil {
				t.Fatal(err)
			}
			if scenario.dead {
				probe.exit(l.req.Runner.Process.Pid)
			}
			h.clock.advance(time.Minute)
			if _, err := h.s.Reconcile(h.ctx); err != nil {
				t.Fatal(err)
			}
			j := h.job(l.id)
			if j.BrokerTermination.GetReason() != scenario.want {
				t.Fatalf("takeover reason = %v, want %v", j.BrokerTermination, scenario.want)
			}
			if scenario.finished && j.State != pb.JobState_JOB_STATE_FINISHED {
				t.Fatalf("completed job still held: %v", j)
			}
		})
	}
}

func TestCancelDeadlineRestartsAfterRecovery(t *testing.T) {
	for _, scenario := range []struct {
		name                      string
		acknowledgeBeforeRestart  bool
		acknowledgeDuringRecovery bool
	}{
		{name: "acknowledgment"},
		{name: "completion acknowledged before restart", acknowledgeBeforeRestart: true},
		{name: "completion acknowledged during recovery", acknowledgeDuringRecovery: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			h, _ := launchHarness(t, 2, &fakeProbe{})
			l := h.granted(2)
			h.start1(&l, 4242)
			if _, err := h.s.Cancel(h.ctx, l.id, nil); err != nil {
				t.Fatal(err)
			}
			var acknowledgedAt *timestamppb.Timestamp
			if scenario.acknowledgeBeforeRestart {
				job, _, err := h.s.AcknowledgeCancel(h.ctx, l.id, l.grant, l.req.Runner)
				if err != nil || job.GetCancelAcknowledgedAt() == nil {
					t.Fatalf("acknowledge before restart: job=%v err=%v", job, err)
				}
				acknowledgedAt = job.GetCancelAcknowledgedAt()
			}
			h.clock.advance(4 * time.Second)
			if err := h.s.Stop(h.ctx); err != nil {
				t.Fatal(err)
			}
			if err := h.store.Close(); err != nil {
				t.Fatal(err)
			}
			var err error
			if h.store, err = storage.Open(h.ctx, h.path); err != nil {
				t.Fatal(err)
			}
			h.clock = &fakeClock{}
			if h.s, _, err = Start(h.ctx, h.store, Options{Config: h.cfg, ServiceVersion: "test", PID: 2,
				Clock: h.clock.read, Sampler: h.sampler, Processes: h.probe}); err != nil {
				t.Fatal(err)
			}
			if scenario.acknowledgeBeforeRestart && !proto.Equal(h.job(l.id).GetCancelAcknowledgedAt(), acknowledgedAt) {
				t.Fatal("restart lost the durable cancellation acknowledgment")
			}
			h.clock.advance(time.Hour)
			if _, err := h.s.Reconcile(h.ctx); err != nil {
				t.Fatal(err)
			}
			if j := h.job(l.id); j.BrokerTermination != nil {
				t.Fatalf("takeover during recovery: %v", j.BrokerTermination)
			}
			if scenario.acknowledgeDuringRecovery {
				if _, _, err := h.s.AcknowledgeCancel(h.ctx, l.id, l.grant, l.req.Runner); err != nil {
					t.Fatal(err)
				}
			}

			h.clock.advance(time.Hour)
			if _, err := h.s.MarkReady(h.ctx); err != nil {
				t.Fatal(err)
			}
			deadline := 5 * time.Second
			reason := pb.BrokerTermination_REASON_CANCEL_ACK_DEADLINE
			if scenario.acknowledgeBeforeRestart || scenario.acknowledgeDuringRecovery {
				deadline = 30 * time.Second
				reason = pb.BrokerTermination_REASON_CANCEL_COMPLETION_DEADLINE
			}
			h.clock.advance(deadline - time.Second)
			if _, err := h.s.Reconcile(h.ctx); err != nil {
				t.Fatal(err)
			}
			if j := h.job(l.id); j.BrokerTermination != nil {
				t.Fatalf("recovery charged old or downtime: %v", j.BrokerTermination)
			}
			h.clock.advance(time.Second)
			if _, err := h.s.Reconcile(h.ctx); err != nil {
				t.Fatal(err)
			}
			if j := h.job(l.id); j.BrokerTermination.GetReason() != reason {
				t.Fatalf("reconstructed deadline takeover = %v, want %v", j.BrokerTermination, reason)
			}
			h.restart(2)
			if j := h.job(l.id); j.BrokerTermination.GetReason() != reason {
				t.Fatalf("restart lost recorded takeover: %v", j.BrokerTermination)
			}
		})
	}
}

func TestConfiguredCancelDeadlines(t *testing.T) {
	h := newHarness(t, 2, func(c *Config) {
		c.CancelAckTimeout = 2 * time.Second
		c.CancelCompletionTimeout = 3 * time.Second
	})
	l := h.granted(2)
	h.start1(&l, 4242)
	if _, err := h.s.Cancel(h.ctx, l.id, nil); err != nil {
		t.Fatal(err)
	}
	h.clock.advance(time.Second)
	if _, _, err := h.s.AcknowledgeCancel(h.ctx, l.id, l.grant, l.req.Runner); err != nil {
		t.Fatal(err)
	}
	h.clock.advance(2 * time.Second)
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if j := h.job(l.id); j.BrokerTermination != nil {
		t.Fatalf("completion deadline expired early: %v", j.BrokerTermination)
	}
	h.clock.advance(time.Second)
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if j := h.job(l.id); j.BrokerTermination.GetReason() != pb.BrokerTermination_REASON_CANCEL_COMPLETION_DEADLINE {
		t.Fatalf("configured completion deadline = %v", j.BrokerTermination)
	}

	h2 := newHarness(t, 2, func(c *Config) { c.CancelAckTimeout = 2 * time.Second })
	l2 := h2.granted(2)
	h2.start1(&l2, 4243)
	if _, err := h2.s.Cancel(h2.ctx, l2.id, nil); err != nil {
		t.Fatal(err)
	}
	h2.clock.advance(2 * time.Second)
	if _, err := h2.s.Reconcile(h2.ctx); err != nil {
		t.Fatal(err)
	}
	if j := h2.job(l2.id); j.BrokerTermination.GetReason() != pb.BrokerTermination_REASON_CANCEL_ACK_DEADLINE {
		t.Fatalf("configured acknowledgment deadline = %v", j.BrokerTermination)
	}
}

func TestStorageFailureCannotStartCancellationTakeover(t *testing.T) {
	h, _ := launchHarness(t, 2, &fakeProbe{})
	l := h.granted(2)
	h.start1(&l, 4242)
	if _, err := h.s.Cancel(h.ctx, l.id, nil); err != nil {
		t.Fatal(err)
	}
	h.clock.advance(5 * time.Second)
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Reconcile(h.ctx); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("takeover during storage failure = %v", err)
	}
	if h.s.Ready() {
		t.Fatal("scheduler stayed ready after storage failure")
	}
	var err error
	if h.store, err = storage.Open(h.ctx, h.path); err != nil {
		t.Fatal(err)
	}
	h.clock = &fakeClock{}
	if h.s, _, err = Start(h.ctx, h.store, Options{Config: h.cfg, ServiceVersion: "test", PID: 2,
		Clock: h.clock.read, Sampler: h.sampler, Processes: h.probe}); err != nil {
		t.Fatal(err)
	}
	if j := h.job(l.id); j.BrokerTermination != nil {
		t.Fatalf("storage failure persisted a takeover: %v", j.BrokerTermination)
	}
	if _, err := h.s.MarkReady(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.clock.advance(4 * time.Second)
	if _, err := h.s.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if j := h.job(l.id); j.BrokerTermination != nil {
		t.Fatalf("reopened store did not restart deadline: %v", j.BrokerTermination)
	}
}
