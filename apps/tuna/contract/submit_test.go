package contract

import (
	"fmt"
	"testing"

	tunapb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

func sampleSubmission() *tunapb.SubmitJobRequest {
	return &tunapb.SubmitJobRequest{
		IdempotencyKey: "key-one",
		Command:        "echo 'hello world'",
		Cwd:            proto.String("/work"),
		Weight:         2,
		Client:         &tunapb.ClientInfo{Name: proto.String("tuna"), Os: tunapb.ClientOS_CLIENT_OS_MACOS},
		Attribution: &tunapb.Attribution{
			Repository: proto.String("example/repo"),
			Extra:      map[string]string{"z": "last", "a": "first"},
		},
		Runner: &tunapb.RunnerIncarnation{
			IncarnationId: "runner",
			Process:       &tunapb.ProcessIdentity{Pid: 10, StartTime: Timestamp(12345)},
		},
	}
}

func TestCanonicalSubmitEquivalence(t *testing.T) {
	original := sampleSubmission()
	equivalent := proto.Clone(original).(*tunapb.SubmitJobRequest)
	equivalent.IdempotencyKey = "another-key"
	equivalent.Attribution.Extra = map[string]string{}
	equivalent.Attribution.Extra["a"] = "first"
	equivalent.Attribution.Extra["z"] = "last"

	want, err := CanonicalSubmitHash(original, AdmissionQueued)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", want); got != "bed23e6c8421c1a80b47c65c491ebb3393a31dc5aa31944fda82a78db4438aec" {
		t.Fatalf("canonical fingerprint changed: %s", got)
	}
	got, err := CanonicalSubmitHash(equivalent, AdmissionQueued)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatal("map order and the lookup key must not change the payload hash")
	}

	withoutMetadata := proto.Clone(original).(*tunapb.SubmitJobRequest)
	withoutMetadata.Client = nil
	withoutMetadata.Attribution = nil
	emptyMetadata := proto.Clone(withoutMetadata).(*tunapb.SubmitJobRequest)
	emptyMetadata.Client = &tunapb.ClientInfo{}
	emptyMetadata.Attribution = &tunapb.Attribution{Extra: map[string]string{}}
	withoutHash, err := CanonicalSubmitHash(withoutMetadata, AdmissionQueued)
	if err != nil {
		t.Fatal(err)
	}
	emptyHash, err := CanonicalSubmitHash(emptyMetadata, AdmissionQueued)
	if err != nil {
		t.Fatal(err)
	}
	if withoutHash != emptyHash {
		t.Fatal("absent and empty attribution/client messages represent the same unknown values")
	}
}

func TestCanonicalSubmitDetectsConflicts(t *testing.T) {
	original := sampleSubmission()
	want, err := CanonicalSubmitHash(original, AdmissionQueued)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mode   AdmissionMode
		change func(*tunapb.SubmitJobRequest)
	}{
		{name: "admission mode", mode: AdmissionNoWait},
		{name: "command", mode: AdmissionQueued, change: func(r *tunapb.SubmitJobRequest) { r.Command += "!" }},
		{name: "optional presence", mode: AdmissionQueued, change: func(r *tunapb.SubmitJobRequest) { r.Cwd = nil }},
		{name: "metadata", mode: AdmissionQueued, change: func(r *tunapb.SubmitJobRequest) { r.Attribution.Extra["a"] = "changed" }},
		{name: "runner incarnation", mode: AdmissionQueued, change: func(r *tunapb.SubmitJobRequest) { r.Runner.Process.StartTime.Nanos++ }},
		{name: "os version", mode: AdmissionQueued, change: func(r *tunapb.SubmitJobRequest) { r.Client.OsVersion = proto.String("26.1") }},
		{name: "empty os version", mode: AdmissionQueued, change: func(r *tunapb.SubmitJobRequest) { r.Client.OsVersion = proto.String("") }},
		{name: "retry lineage", mode: AdmissionQueued, change: func(r *tunapb.SubmitJobRequest) { r.RetryOfJobId = proto.String("predecessor") }},
		{name: "priority", mode: AdmissionQueued, change: func(r *tunapb.SubmitJobRequest) { r.Priority = proto.Int32(-1) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := proto.Clone(original).(*tunapb.SubmitJobRequest)
			if tt.change != nil {
				tt.change(changed)
			}
			got, err := CanonicalSubmitHash(changed, tt.mode)
			if err != nil {
				t.Fatal(err)
			}
			if got == want {
				t.Fatal("a different submission payload must not be equivalent")
			}
		})
	}
}

func TestCanonicalSubmitDefaultPriorityMatchesAbsent(t *testing.T) {
	request := sampleSubmission()
	absent, err := CanonicalSubmitHash(request, AdmissionQueued)
	if err != nil {
		t.Fatal(err)
	}
	request.Priority = proto.Int32(1337)
	explicit, err := CanonicalSubmitHash(request, AdmissionQueued)
	if err != nil {
		t.Fatal(err)
	}
	if absent != explicit {
		t.Fatal("explicit default must match absent priority")
	}
}

func TestCanonicalSubmitRejectsUnknownOrInvalidInput(t *testing.T) {
	unknown := sampleSubmission()
	unknown.Attribution.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01})
	if _, err := CanonicalSubmitHash(unknown, AdmissionQueued); err == nil {
		t.Fatal("unknown nested fields must not be silently ignored")
	}
	unknown = sampleSubmission()
	unknown.Runner.Process.StartTime.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01})
	if _, err := CanonicalSubmitHash(unknown, AdmissionQueued); err == nil {
		t.Fatal("unknown timestamp fields must not be silently ignored")
	}
	if _, err := CanonicalSubmitHash(sampleSubmission(), AdmissionMode(99)); err == nil {
		t.Fatal("unknown admission mode must be rejected")
	}
	if _, err := CanonicalSubmitHash(nil, AdmissionQueued); err == nil {
		t.Fatal("nil submission must be rejected")
	}
}

func TestCanonicalSubmitRejectsNewPopulatedFields(t *testing.T) {
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    proto.String("future.proto"),
		Package: proto.String("tuna"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("SubmitJobRequest"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name: proto.String("new_field"), Number: proto.Int32(10),
				Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			}},
		}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	message := dynamicpb.NewMessage(file.Messages().Get(0))
	field := message.Descriptor().Fields().Get(0)
	message.Set(field, protoreflect.ValueOfString("new semantics"))
	if !hasUnknownFields(message.ProtoReflect()) {
		t.Fatal("new populated contract fields must not be silently omitted from the persisted hash")
	}
}

func TestRenderCommand(t *testing.T) {
	got, err := RenderCommand([]string{"git", "commit", "-m", "it's $ready", ""})
	if err != nil {
		t.Fatal(err)
	}
	want := "git commit -m 'it'\\''s $ready' ''"
	if got != want {
		t.Fatalf("rendered command = %q, want %q", got, want)
	}
	for _, args := range [][]string{nil, {"echo", "bad\x00argument"}, {"echo", string([]byte{0xff})}} {
		if _, err := RenderCommand(args); err == nil {
			t.Fatalf("invalid argv %#v must be rejected", args)
		}
	}
}
