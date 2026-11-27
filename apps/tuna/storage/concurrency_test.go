package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/boumenot/tuna/apps/tuna/contract"
	pb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
	"google.golang.org/protobuf/proto"
)

func TestConcurrentSchemaInitialization(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "state.db")
	start := make(chan struct{})
	done := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			store, err := Open(ctx, path)
			if err == nil {
				err = store.Close()
			}
			done <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestConcurrentDeleteRetryAndRetransmission(t *testing.T) {
	for _, scenario := range []string{"retry then delete", "delete then retry", "retry then duplicate", "retry then conflict", "retry then distinct retry", "delete then resubmit"} {
		t.Run(scenario, func(t *testing.T) {
			f := setup(t)
			req := f.request()
			parent := f.submit(req, contract.AdmissionNoWait, &Admission{}).Job
			f.finish(parent, req, 1)
			retry := f.request()
			retry.RetryOfJobId = &parent.JobId
			ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
			defer cancel()
			other, err := Open(ctx, f.path)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			held, release, attempting := make(chan struct{}), make(chan struct{}), make(chan struct{})
			firstDone := make(chan error, 1)
			secondDone := make(chan error, 1)
			var child string
			go func() {
				_, err := f.store.Write(ctx, func(tx *Tx) error {
					if scenario == "delete then retry" || scenario == "delete then resubmit" {
						if err := tx.DeleteJob(parent.JobId); err != nil {
							return err
						}
					} else {
						result, err := tx.Submit(retry, Submission{Mode: contract.AdmissionQueued, ServiceVersion: "test", ServiceRunID: f.run})
						if err != nil {
							return err
						}
						child = result.Job.JobId
					}
					close(held)
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
				firstDone <- err
			}()
			select {
			case <-held:
			case err := <-firstDone:
				t.Fatalf("first writer: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			go func() {
				close(attempting)
				_, err := other.Write(ctx, func(tx *Tx) error {
					if scenario == "retry then delete" {
						return tx.DeleteJob(parent.JobId)
					}
					next := retry
					if scenario == "retry then distinct retry" {
						next = f.request()
						next.RetryOfJobId = &parent.JobId
					}
					if scenario == "retry then conflict" {
						next = proto.Clone(retry).(*pb.SubmitJobRequest)
						next.Command = "different"
					}
					if scenario == "delete then resubmit" {
						next = req
					}
					result, err := tx.Submit(next, Submission{Mode: contract.AdmissionQueued, ServiceVersion: "test", ServiceRunID: f.run})
					if err == nil && scenario == "retry then duplicate" && (result.Created || result.Job.JobId != child) {
						return errors.New("duplicate did not return committed retry")
					}
					return err
				})
				secondDone <- err
			}()
			<-attempting
			close(release)
			if err := <-firstDone; err != nil {
				t.Fatal(err)
			}
			err = <-secondDone
			var want error
			switch scenario {
			case "delete then retry", "retry then distinct retry":
				want = ErrRetryNotEligible
			case "retry then conflict":
				want = ErrKeyReused
			case "delete then resubmit":
				want = ErrKeyDeleted
			}
			if !errors.Is(err, want) {
				t.Fatalf("race result: got %v want %v", err, want)
			}
			if scenario == "retry then delete" {
				j := f.job(child)
				if j.GetRetryOfJobId() != parent.JobId || j.Attempt != 2 || j.State != pb.JobState_JOB_STATE_QUEUED {
					t.Fatalf("surviving retry: %v", j)
				}
			}
		})
	}
}
