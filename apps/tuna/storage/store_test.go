package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/boumenot/tuna/apps/tuna/contract"
	pb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
)

func TestSubmissionCommitAndRollback(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var run int64
	_, err = store.Write(ctx, func(tx *Tx) error {
		var err error
		run, err = tx.StartService("test", 1)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	request := &pb.SubmitJobRequest{
		IdempotencyKey: "11111111-1111-4111-8111-111111111111",
		Command:        "example", Weight: 1,
		Runner: &pb.RunnerIncarnation{
			IncarnationId: "22222222-2222-4222-8222-222222222222",
			Process:       &pb.ProcessIdentity{Pid: 1, StartTime: contract.Timestamp(1)},
		},
	}
	options := Submission{Mode: contract.AdmissionQueued, ServiceVersion: "test", ServiceRunID: run}
	abort := errors.New("abort")
	events, err := store.Write(ctx, func(tx *Tx) error {
		if _, err := tx.Submit(request, options); err != nil {
			return err
		}
		return abort
	})
	if !errors.Is(err, abort) || len(events) != 0 {
		t.Fatalf("rollback returned events=%v err=%v", events, err)
	}
	err = store.View(ctx, func(r *Reader) error {
		_, err := r.Submission(request.IdempotencyKey)
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("uncommitted job visible: %v", err)
	}
	var first SubmissionResult
	events, err = store.Write(ctx, func(tx *Tx) error {
		var err error
		first, err = tx.Submit(request, options)
		return err
	})
	if err != nil || !first.Created || len(events) != 2 {
		t.Fatalf("submit: %+v events=%v err=%v", first, events, err)
	}
	events, err = store.Write(ctx, func(tx *Tx) error {
		result, err := tx.Submit(request, options)
		if err == nil && (result.Created || result.Job.JobId != first.Job.JobId) {
			t.Fatalf("duplicate: %+v", result)
		}
		return err
	})
	if err != nil || len(events) != 0 {
		t.Fatalf("duplicate emitted change: events=%v err=%v", events, err)
	}
	request.Command = "different"
	_, err = store.Write(ctx, func(tx *Tx) error {
		_, err := tx.Submit(request, options)
		return err
	})
	if !errors.Is(err, ErrKeyReused) {
		t.Fatalf("conflict: %v", err)
	}
}
