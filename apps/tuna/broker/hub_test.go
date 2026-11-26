package broker

import (
	"testing"

	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
)

func TestHubDeliversInOrderAndOverflowsWithoutBlocking(t *testing.T) {
	h := NewHub()
	fast := h.Subscribe(8)
	slow := h.Subscribe(1)
	gone := h.Subscribe(1)
	gone.Close()
	gone.Close()
	h.Publish([]*pb.BrokerEvent{{EventSeq: 1}, {EventSeq: 2}})
	h.Publish([]*pb.BrokerEvent{{EventSeq: 3}})
	for want := int64(1); want <= 3; want++ {
		if got := (<-fast.Events()).EventSeq; got != want {
			t.Fatalf("event %d, want %d", got, want)
		}
	}
	select {
	case <-fast.Overflowed():
		t.Fatal("subscriber with room overflowed")
	default:
	}
	<-slow.Overflowed()
	if got := (<-slow.Events()).EventSeq; got != 1 {
		t.Fatalf("slow subscriber got %d before overflow", got)
	}
	select {
	case ev := <-slow.Events():
		t.Fatalf("overflowed subscriber received %d", ev.EventSeq)
	case <-gone.Events():
		t.Fatal("closed subscriber received an event")
	default:
	}
}
