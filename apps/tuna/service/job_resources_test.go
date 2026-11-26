package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"github.com/boumenot/heavy-lock/apps/tuna/jobmetrics"
	"github.com/boumenot/heavy-lock/apps/tuna/storage"
	"google.golang.org/protobuf/proto"
)

type resourceTestScheduler struct {
	jobs   map[string]*pb.Job
	writes []*pb.ResourceSummary
}

func (s *resourceTestScheduler) Snapshot(context.Context) (*pb.BrokerSnapshot, error) {
	snapshot := &pb.BrokerSnapshot{}
	for _, job := range s.jobs {
		if job.State != pb.JobState_JOB_STATE_FINISHED {
			snapshot.ActiveJobs = append(snapshot.ActiveJobs, proto.Clone(job).(*pb.Job))
		}
	}
	return snapshot, nil
}

func (s *resourceTestScheduler) Job(_ context.Context, id string) (*pb.Job, error) {
	if job := s.jobs[id]; job != nil {
		return proto.Clone(job).(*pb.Job), nil
	}
	return nil, storage.ErrNotFound
}

func (s *resourceTestScheduler) SaveResources(_ context.Context, id string, summary *pb.ResourceSummary, finished bool) error {
	job := s.jobs[id]
	if job == nil {
		return storage.ErrNotFound
	}
	if (job.State == pb.JobState_JOB_STATE_FINISHED) != finished {
		return storage.ErrTransition
	}
	job.Resources = proto.Clone(summary).(*pb.ResourceSummary)
	s.writes = append(s.writes, proto.Clone(summary).(*pb.ResourceSummary))
	return nil
}

type resourceTestSource func(context.Context) (jobmetrics.Snapshot, error)

func (s resourceTestSource) Snapshot(ctx context.Context) (jobmetrics.Snapshot, error) {
	return s(ctx)
}

func testResourceJob() *pb.Job {
	return &pb.Job{JobId: "job", State: pb.JobState_JOB_STATE_RUNNING,
		Process: &pb.ProcessIdentity{Pid: 100, StartTimeUnixNs: 10}}
}

func testResourceSample(cpu time.Duration, rss *int64) jobmetrics.Snapshot {
	nanos := int64(cpu)
	return jobmetrics.Snapshot{Processes: []jobmetrics.Process{{
		Identity:  jobmetrics.Identity{PID: 100, StartTimeUnixNs: 10},
		ParentPID: 1, CPUTimeNs: &nanos, RSSBytes: rss,
	}}}
}

func TestResourceManagerCheckpointsAndRejectedFinish(t *testing.T) {
	ctx := context.Background()
	job := testResourceJob()
	sched := &resourceTestScheduler{jobs: map[string]*pb.Job{job.JobId: job}}
	var now time.Duration
	var calls int
	rss := int64(100)
	source := resourceTestSource(func(context.Context) (jobmetrics.Snapshot, error) {
		calls++
		return testResourceSample(time.Duration(calls)*time.Second, &rss), nil
	})
	m := newResourceManager(sched, source)
	m.clock = func() time.Duration { return now }
	for i := 0; i <= 12; i++ {
		now = time.Duration(i) * 5 * time.Second
		if err := m.sample(ctx); err != nil {
			t.Fatal(err)
		}
		if i < 12 && len(sched.writes) != 0 {
			t.Fatalf("checkpoint written before 60 seconds: %+v", sched.writes)
		}
	}
	if calls != 13 || len(sched.writes) != 1 || sched.writes[0].SampleCount != 13 ||
		sched.writes[0].GetCpuTimeMs() != 13000 || sched.writes[0].GetPeakCpuPercent() != 20 {
		t.Fatalf("five-second observations/checkpoint: calls=%d writes=%+v", calls, sched.writes)
	}

	before := m.entries[job.JobId].aggregate.Summary()
	req := &pb.ReportFinishedRequest{JobId: job.JobId, Process: job.Process,
		ElapsedMs: proto.Int64(62000), RootCpuTimeMs: proto.Int64(14000)}
	rejected := errors.New("report rejected")
	_, err := m.Finish(ctx, req, func(*pb.ResourceSummary) (*pb.Job, error) {
		return nil, rejected
	})
	if !errors.Is(err, rejected) || !proto.Equal(before, m.entries[job.JobId].aggregate.Summary()) ||
		!proto.Equal(sched.writes[0], job.Resources) {
		t.Fatal("rejected finish changed the live or durable summary")
	}
	result, err := m.Finish(ctx, req, func(summary *pb.ResourceSummary) (*pb.Job, error) {
		job.Resources = proto.Clone(summary).(*pb.ResourceSummary)
		job.State = pb.JobState_JOB_STATE_FINISHING
		return proto.Clone(job).(*pb.Job), nil
	})
	if err != nil || result.Resources.GetElapsedMs() != 62000 || result.Resources.GetCpuTimeMs() != 14000 ||
		result.Resources.GetSampleCount() != 13 {
		t.Fatalf("committed finish = %+v, %v", result, err)
	}
}

func TestResourceManagerRecoveryAndObservationFailures(t *testing.T) {
	ctx := context.Background()
	job := testResourceJob()
	job.Resources = &pb.ResourceSummary{CpuTimeMs: proto.Int64(10000), PeakCpuPercent: proto.Float64(150),
		PeakCombinedRssBytes: proto.Int64(100), SampleCount: 5}
	sched := &resourceTestScheduler{jobs: map[string]*pb.Job{job.JobId: job}}
	var now time.Duration
	var calls int
	rss := int64(20)
	source := resourceTestSource(func(context.Context) (jobmetrics.Snapshot, error) {
		calls++
		switch calls {
		case 1:
			return testResourceSample(7*time.Second, &rss), nil
		case 2:
			return testResourceSample(9*time.Second, &rss), nil
		case 3:
			return jobmetrics.Snapshot{}, errors.New("permission denied")
		case 4:
			return testResourceSample(12*time.Second, nil), nil
		default:
			return testResourceSample(13*time.Second, &rss), nil
		}
	})
	m := newResourceManager(sched, source)
	m.clock = func() time.Duration { return now }
	if err := m.restore(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sched.writes) != 1 || !strings.Contains(sched.writes[0].GetIncompletenessReason(), "restart") {
		t.Fatalf("recovery did not immediately mark the checkpoint partial: %+v", sched.writes)
	}
	for i := 0; i < 4; i++ {
		now = time.Duration(i) * 5 * time.Second
		err := m.sample(ctx)
		if (i == 2) != (err != nil) {
			t.Fatalf("sample %d: %v", i, err)
		}
	}
	s := m.entries[job.JobId].aggregate.Summary()
	if s.GetCpuTimeMs() != 15000 || s.GetPeakCpuPercent() != 150 ||
		s.GetPeakCombinedRssBytes() != 100 || s.SampleCount != 8 ||
		!strings.Contains(s.GetIncompletenessReason(), "permission denied") ||
		!strings.Contains(s.GetIncompletenessReason(), "RSS was unavailable") {
		t.Fatalf("recovered/failing observations = %+v", s)
	}
	now = 60 * time.Second
	if err := m.sample(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sched.writes) != 2 || sched.writes[1].GetCpuTimeMs() != 16000 ||
		!strings.Contains(sched.writes[1].GetIncompletenessReason(), "sampling was delayed") {
		t.Fatalf("recovered checkpoint = %+v", sched.writes)
	}

	emptyJob := testResourceJob()
	emptySched := &resourceTestScheduler{jobs: map[string]*pb.Job{emptyJob.JobId: emptyJob}}
	unavailable := newResourceManager(emptySched, resourceTestSource(func(context.Context) (jobmetrics.Snapshot, error) {
		return jobmetrics.Snapshot{}, errors.New("enumeration failed")
	}))
	if err := unavailable.sample(ctx); err == nil {
		t.Fatal("enumeration failure was not surfaced")
	}
	s = unavailable.entries[emptyJob.JobId].aggregate.Summary()
	if s.CpuTimeMs != nil || s.PeakCombinedRssBytes != nil ||
		s.Completeness != pb.MeasurementCompleteness_MEASUREMENT_COMPLETENESS_UNAVAILABLE {
		t.Fatalf("unavailable values were fabricated: %+v", s)
	}
}
