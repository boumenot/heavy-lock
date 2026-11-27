package broker

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"github.com/boumenot/heavy-lock/apps/tuna/scheduler"
	"github.com/boumenot/heavy-lock/apps/tuna/storage"
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

type testSampler struct{}

func (testSampler) Sample(context.Context) (scheduler.Sample, error) {
	return scheduler.Sample{}, nil
}
