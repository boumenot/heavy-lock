package service

import (
	"context"

	"github.com/boumenot/heavy-lock/apps/tuna/broker"
	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"github.com/boumenot/heavy-lock/apps/tuna/scheduler"
	"github.com/boumenot/heavy-lock/apps/tuna/storage"
)

// processProbe is nil until runner process verification (#13) is composed;
// the scheduler then never verifies an exit and keeps capacity held.
var processProbe scheduler.ProcessProbe

// composite serves both gRPC services from one scheduler and store.
//
// Integration with #13: embed its broker.Runner (built from the same
// scheduler and hub) in place of UnimplementedRunnerServiceServer and set
// processProbe to its verified process probe.
type composite struct {
	*broker.Control
	pb.UnimplementedRunnerServiceServer
}

func newBroker(sched *scheduler.Scheduler, store *storage.Store, hub *broker.Hub, o Options) (*composite, error) {
	control, err := broker.NewControl(sched, store, broker.Options{
		Hub:            hub,
		ServiceVersion: o.ServiceVersion,
		EventMaxAge:    o.EventMaxAge,
		EventMaxCount:  o.EventMaxCount,
	})
	if err != nil {
		return nil, err
	}
	return &composite{Control: control}, nil
}

func (c *composite) maintain(ctx context.Context) error { return c.Control.Maintain(ctx) }
