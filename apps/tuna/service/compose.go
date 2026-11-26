package service

import (
	"context"

	"github.com/boumenot/heavy-lock/apps/tuna/broker"
	"github.com/boumenot/heavy-lock/apps/tuna/jobmetrics"
	"github.com/boumenot/heavy-lock/apps/tuna/scheduler"
	"github.com/boumenot/heavy-lock/apps/tuna/storage"
)

// composite serves both gRPC services from one scheduler, store and hub.
type composite struct {
	*broker.Control
	*broker.Runner
	resources *resourceManager
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
	source := o.JobSampler
	if source == nil {
		source = jobmetrics.DefaultSource()
	}
	resources := newResourceManager(sched, source)
	runner, err := broker.NewRunner(sched, hub, broker.RunnerOptions{Resources: resources})
	if err != nil {
		return nil, err
	}
	return &composite{Control: control, Runner: runner, resources: resources}, nil
}

func (c *composite) maintain(ctx context.Context) error { return c.Control.Maintain(ctx) }
