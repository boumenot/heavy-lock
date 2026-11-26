package scheduler

import (
	"errors"
	"sync"
	"testing"
	"time"

	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"github.com/boumenot/heavy-lock/apps/tuna/storage"
)

func TestRaceSameKeyNoWaitHasOneOutcome(t *testing.T) {
	for _, capacity := range []uint32{1, 4} {
		h := newHarness(t, capacity, nil)
		h.queued(1) // capacity 1 forces busy; capacity 4 admits
		req := request(t, 1)
		var wg sync.WaitGroup
		ids := make([]string, 16)
		errs := make([]error, 16)
		for i := range ids {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, j, _, err := h.s.TrySubmitAndWatch(h.ctx, req)
				errs[i] = err
				if j != nil {
					ids[i] = j.JobId
				}
			}()
		}
		wg.Wait()
		for i := range ids {
			if !errors.Is(errs[i], errs[0]) || ids[i] != ids[0] {
				t.Fatalf("capacity %d: inconsistent outcomes %v/%q vs %v/%q", capacity, errs[i], ids[i], errs[0], ids[0])
			}
		}
		if busy := errors.Is(errs[0], storage.ErrAdmissionBusy); busy != (capacity == 1) || !busy && errs[0] != nil {
			t.Fatalf("capacity %d: outcome %v", capacity, errs[0])
		}
	}
}

func TestRaceConcurrentAdmissionsNeverExceedCapacity(t *testing.T) {
	h := newHarness(t, 4, nil)
	var wg sync.WaitGroup
	for i := range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			weight := uint32(i%3 + 1)
			if i%2 == 0 {
				_, _, _, err := h.s.TrySubmitAndWatch(h.ctx, request(t, weight))
				if err != nil && !errors.Is(err, storage.ErrAdmissionBusy) {
					t.Error(err)
				}
				return
			}
			req := request(t, weight)
			result, _, err := h.s.Submit(h.ctx, req)
			if err == nil {
				_, _, _, err = h.s.Watch(h.ctx, result.Job.JobId, req.Runner)
			}
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	snap := h.snapshot()
	var sum uint32
	for _, j := range snap.ActiveJobs {
		sum += j.Weight
	}
	if snap.Status.Occupied > 4 || sum != snap.Status.Occupied || snap.Status.Occupied == 0 {
		t.Fatalf("occupied %d (active weight %d) with capacity 4", snap.Status.Occupied, sum)
	}
}

func TestRaceReconnectAndExpiryNeverRevive(t *testing.T) {
	for range 20 {
		h := newHarness(t, 1, nil)
		if _, err := h.s.Pause(h.ctx, true); err != nil {
			t.Fatal(err)
		}
		req, id := h.submit(1)
		h.clock.advance(31 * time.Second)
		var wg sync.WaitGroup
		var reg *Registration
		var watchErr error
		wg.Add(2)
		go func() { defer wg.Done(); reg, _, _, watchErr = h.s.Watch(h.ctx, id, req.Runner) }()
		go func() {
			defer wg.Done()
			if _, err := h.s.Tick(h.ctx); err != nil {
				t.Error(err)
			}
		}()
		wg.Wait()
		j := h.job(id)
		switch {
		case watchErr != nil:
			t.Fatal(watchErr)
		case reg != nil:
			if j.State != pb.JobState_JOB_STATE_QUEUED || !j.RunnerConnected {
				t.Fatalf("registered stream for %v connected=%v", j.State, j.RunnerConnected)
			}
		case j.State != pb.JobState_JOB_STATE_FINISHED || j.Outcome.Kind != pb.OutcomeKind_OUTCOME_KIND_REMOVED_DISCONNECTED:
			t.Fatalf("no registration but job %v", j.State)
		}
	}
}
