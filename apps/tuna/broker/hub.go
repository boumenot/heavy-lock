package broker

import (
	"sync"

	pb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
)

// Hub fans committed broker events out to live subscribers in commit order.
// Publish never blocks: a subscriber whose buffer is full is marked
// overflowed and receives nothing further; it must recover from durable
// replay or a fresh snapshot. Events are shared and must not be mutated.
type Hub struct {
	mu   sync.Mutex
	subs map[*Subscription]struct{}
}

func NewHub() *Hub { return &Hub{subs: map[*Subscription]struct{}{}} }

// Subscription is one live subscriber. Subscribe before reading a replay or
// snapshot cursor and skip events with EventSeq at or below it, so delivery
// has no gap.
type Subscription struct {
	hub      *Hub
	events   chan *pb.BrokerEvent
	overflow chan struct{}
	once     sync.Once
	full     bool
}

// Publish delivers committed events. The scheduler calls it under its
// mutex right after each commit, so calls are totally ordered.
func (h *Hub) Publish(events []*pb.BrokerEvent) {
	if h == nil || len(events) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		for _, event := range events {
			if s.full {
				break
			}
			select {
			case s.events <- event:
			default:
				s.full = true
				close(s.overflow)
			}
		}
	}
}

// Subscribe registers a subscriber with the given buffer (at least 1).
func (h *Hub) Subscribe(buffer int) *Subscription {
	s := &Subscription{hub: h, events: make(chan *pb.BrokerEvent, max(buffer, 1)), overflow: make(chan struct{})}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subs[s] = struct{}{}
	return s
}

// Events is never closed by the hub.
func (s *Subscription) Events() <-chan *pb.BrokerEvent { return s.events }

// Overflowed is closed once the subscriber fell behind and lost events.
func (s *Subscription) Overflowed() <-chan struct{} { return s.overflow }

// Close unsubscribes; it is idempotent.
func (s *Subscription) Close() {
	s.once.Do(func() {
		s.hub.mu.Lock()
		defer s.hub.mu.Unlock()
		delete(s.hub.subs, s)
	})
}
