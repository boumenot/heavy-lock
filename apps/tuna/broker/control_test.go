package broker

import (
	"context"
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
	if err != nil || !st.Paused {
		t.Fatalf("pause = %v, %v", st, err)
	}
	st, err = c.ResumeQueue(ctx, &pb.ResumeQueueRequest{})
	if err != nil || st.Paused {
		t.Fatalf("resume = %v, %v", st, err)
	}
}

type testSampler struct{}

func (testSampler) Sample(context.Context) (scheduler.Sample, error) {
	return scheduler.Sample{}, nil
}
