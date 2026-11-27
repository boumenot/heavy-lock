package broker

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/boumenot/heavy-lock/apps/tuna/contract"
	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"github.com/boumenot/heavy-lock/apps/tuna/scheduler"
	"github.com/boumenot/heavy-lock/apps/tuna/storage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestControlReadinessPauseAndSnapshot(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "broker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	hub := NewHub()
	s, _, err := scheduler.Start(ctx, store, scheduler.Options{
		Config: scheduler.DefaultConfig(2), ServiceVersion: "test", PID: 123,
		Sampler: testSampler{}, Publish: hub.Publish,
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewControl(s, store, Options{ServiceVersion: "test", Hub: hub})
	if err != nil {
		t.Fatal(err)
	}
	h, err := c.GetHealth(ctx, &pb.GetHealthRequest{})
	if err != nil || h.Ready || !h.Live {
		t.Fatalf("health = %v, %v", h, err)
	}
	if _, err := s.MarkReady(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := c.PauseQueue(ctx, &pb.PauseQueueRequest{})
	if err != nil || st.QueueMode != pb.QueueMode_QUEUE_MODE_PAUSED {
		t.Fatalf("pause = %v, %v", st, err)
	}
	st, err = c.ResumeQueue(ctx, &pb.ResumeQueueRequest{})
	if err != nil || st.QueueMode == pb.QueueMode_QUEUE_MODE_PAUSED {
		t.Fatalf("resume = %v, %v", st, err)
	}
}

func TestControlRejectsInvalidTimestampBounds(t *testing.T) {
	c := &Control{}
	for _, request := range []*pb.ListJobsRequest{
		{QueuedAfter: &timestamppb.Timestamp{Seconds: 9223372036, Nanos: 854775808}},
		{QueuedBefore: &timestamppb.Timestamp{Seconds: 0, Nanos: -1}},
		{QueuedAfter: contract.Timestamp(2), QueuedBefore: contract.Timestamp(1)},
	} {
		if _, err := c.ListJobs(context.Background(), request); status.Code(err) != codes.InvalidArgument {
			t.Errorf("ListJobs(%v): %v", request, err)
		}
	}
	if _, err := c.DeleteHistory(context.Background(), &pb.DeleteHistoryRequest{
		FinishedBefore: &timestamppb.Timestamp{Seconds: -9223372037, Nanos: 145224191},
	}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("DeleteHistory: %v", err)
	}
}

func TestGetStatusReportsErrorWhenStorageFails(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "broker.db")
	store, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s, _, err := scheduler.Start(ctx, store, scheduler.Options{
		Config: scheduler.DefaultConfig(3), ServiceVersion: "test", PID: 123, Sampler: testSampler{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkReady(ctx); err != nil {
		t.Fatal(err)
	}
	c, err := NewControl(s, store, Options{ServiceVersion: "test", Hub: NewHub()})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TRIGGER fail_writes BEFORE UPDATE ON broker_state BEGIN SELECT RAISE(ABORT, 'injected'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PauseQueue(ctx, &pb.PauseQueueRequest{}); err == nil || store.Failure() == nil {
		t.Fatalf("injected write failure did not fail storage: %v", err)
	}
	st, err := c.GetStatus(ctx, &pb.GetStatusRequest{})
	if err != nil || st.QueueState != pb.QueueState_QUEUE_STATE_ERROR || !st.StorageUnavailable ||
		st.QueueMode != pb.QueueMode_QUEUE_MODE_UNSPECIFIED || st.Capacity != 3 ||
		st.HeadBlockedReason != pb.BlockedReason_BLOCKED_REASON_STORAGE_UNAVAILABLE {
		t.Fatalf("degraded status = %v, %v", st, err)
	}
}

func TestWatchEventsEndsWhenStorageFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "broker.db")
	store, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s, _, err := scheduler.Start(ctx, store, scheduler.Options{
		Config: scheduler.DefaultConfig(2), ServiceVersion: "test", PID: 123, Sampler: testSampler{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkReady(ctx); err != nil {
		t.Fatal(err)
	}
	c, err := NewControl(s, store, Options{ServiceVersion: "test", Hub: NewHub(), HeartbeatInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := make(chan struct{})
	stream := &eventStream{ctx: ctx, send: func(event *pb.BrokerEvent) error {
		if event.GetSnapshot() != nil {
			close(snapshot)
		}
		return nil
	}}
	done := make(chan error, 1)
	go func() { done <- c.WatchEvents(&pb.WatchEventsRequest{}, stream) }()
	<-snapshot
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TRIGGER fail_writes BEFORE UPDATE ON broker_state BEGIN SELECT RAISE(ABORT, 'injected'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PauseQueue(ctx, &pb.PauseQueueRequest{}); err == nil || store.Failure() == nil {
		t.Fatalf("injected write failure did not fail storage: %v", err)
	}
	// A live watcher must learn of the failure; heartbeats alone would keep
	// clients showing stale live state.
	select {
	case err := <-done:
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("watch ended with %v, want Unavailable", err)
		}
	case <-ctx.Done():
		t.Fatal("watch kept streaming after storage failed")
	}
}

type testSampler struct{}

func (testSampler) Sample(context.Context) (scheduler.Sample, error) {
	return scheduler.Sample{}, nil
}
