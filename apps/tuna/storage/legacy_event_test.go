package storage

import (
	"testing"

	"github.com/boumenot/tuna/apps/tuna/contract"
	pb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func legacyVarint(dst []byte, field protowire.Number, value int64) []byte {
	dst = protowire.AppendTag(dst, field, protowire.VarintType)
	return protowire.AppendVarint(dst, uint64(value))
}

func TestLegacyEventReplayRejectsAmbiguousAndMalformedFields(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
	}{
		{"duplicate old timestamp", legacyVarint(legacyVarint(nil, 2, 1), 2, 2)},
		{"wrong timestamp wire type", protowire.AppendFixed32(protowire.AppendTag(nil, 2, protowire.Fixed32Type), 1)},
		{"negative legacy duration", legacyBytes(nil, 11, legacyBytes(nil, 23, legacyVarint(nil, 1, -1)))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := &pb.BrokerEvent{}
			if err := proto.Unmarshal(tt.payload, event); err != nil {
				t.Fatal(err)
			}
			if err := restoreLegacyEvent(event.ProtoReflect()); err == nil {
				t.Fatalf("accepted malformed legacy event %x", tt.payload)
			}
		})
	}
	event := &pb.BrokerEvent{At: contract.Timestamp(2)}
	event.ProtoReflect().SetUnknown(legacyVarint(nil, 2, 1))
	if err := restoreLegacyEvent(event.ProtoReflect()); err == nil {
		t.Fatal("accepted both old and new timestamps")
	}
}

func legacyBytes(dst []byte, field protowire.Number, value []byte) []byte {
	dst = protowire.AppendTag(dst, field, protowire.BytesType)
	return protowire.AppendBytes(dst, value)
}

func TestLegacyEventReplayPreservesTimesAndMeasurements(t *testing.T) {
	const at = int64(1764121200123456789)
	const queued = int64(1764121199123456789)
	const processStart = int64(1764121199234567891)
	process := legacyVarint(nil, 1, 42)
	process = legacyVarint(process, 2, processStart)
	resources := legacyVarint(nil, 1, 0)
	resources = legacyVarint(resources, 2, 1234)
	job := legacyBytes(nil, 1, []byte("legacy-job"))
	job = legacyVarint(job, 11, queued)
	job = legacyBytes(job, 20, process)
	job = legacyBytes(job, 23, resources)
	payload := legacyVarint(nil, 1, 2)
	payload = legacyVarint(payload, 2, at)
	payload = legacyBytes(payload, 11, job)

	f := setup(t)
	f.write(func(tx *Tx) error { return tx.SetQueueMode(pb.QueueMode_QUEUE_MODE_PAUSED) })
	if _, err := f.store.db.Exec("UPDATE events SET at_ns=?,kind='job_changed',job_id=?,payload=? WHERE seq=2",
		at, "legacy-job", payload); err != nil {
		t.Fatal(err)
	}
	var events []*pb.BrokerEvent
	err := f.store.View(f.ctx, func(r *Reader) error {
		var err error
		events, err = r.Events(1, 10)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("replayed %d events", len(events))
	}
	event := events[0]
	gotAt, err := contract.UnixNano(event.At)
	if err != nil || gotAt != at {
		t.Fatalf("event time = %d, %v", gotAt, err)
	}
	got := event.GetJobChanged()
	if got == nil || got.Process == nil || got.Resources == nil {
		t.Fatalf("legacy job payload lost: %v", event)
	}
	gotQueued, err := contract.UnixNano(got.QueuedAt)
	if err != nil || gotQueued != queued {
		t.Fatalf("queued time = %d, %v", gotQueued, err)
	}
	gotStart, err := contract.UnixNano(got.Process.StartTime)
	if err != nil || gotStart != processStart {
		t.Fatalf("process start = %d, %v", gotStart, err)
	}
	elapsed, elapsedErr := contract.Milliseconds(got.Resources.Elapsed)
	cpu, cpuErr := contract.Milliseconds(got.Resources.CpuTime)
	if elapsedErr != nil || cpuErr != nil || elapsed != 0 || cpu != 1234 {
		t.Fatalf("measurements elapsed=%d (%v), cpu=%d (%v)", elapsed, elapsedErr, cpu, cpuErr)
	}
}
