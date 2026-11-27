package tunapb

import (
	"encoding/hex"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// submitJobHex is the wire encoding of sampleSubmitJob. The Swift bindings
// test in packages/tuna-contract-swift asserts the same bytes.
const submitJobHex = "0a2431623461323765632d376537352d346333612d396436322d33323236326538613836343812096461727420746573" +
	"741a042f72657020022a0d0a0474756e611205302e302e3132150a0c626f756d656e6f742f7265702a056275696c643a" +
	"260a2430663162633533632d396135612d343165652d613232622d643462363139303734666233"

func sampleSubmitJob() *SubmitJobRequest {
	return &SubmitJobRequest{
		IdempotencyKey: "1b4a27ec-7e75-4c3a-9d62-32262e8a8648",
		Command:        "dart test",
		Cwd:            proto.String("/rep"),
		Weight:         2,
		Client:         &ClientInfo{Name: proto.String("tuna"), Version: proto.String("0.0.1")},
		Attribution:    &Attribution{Repository: proto.String("boumenot/rep"), Purpose: proto.String("build")},
		Runner:         &RunnerIncarnation{IncarnationId: "0f1bc53c-9a5a-41ee-a22b-d4b619074fb3"},
	}
}

func TestSubmitJobRoundTrip(t *testing.T) {
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(sampleSubmitJob())
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(encoded); got != submitJobHex {
		t.Fatalf("encoding = %s, want %s", got, submitJobHex)
	}
	var decoded SubmitJobRequest
	if err := proto.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(&decoded, sampleSubmitJob()) {
		t.Fatalf("decoded = %v", &decoded)
	}
}

func TestFinishMeasurementsMatchSwiftEncoding(t *testing.T) {
	report := &ReportFinishedRequest{JobId: "job", GrantId: "grant",
		Process: &ProcessIdentity{Pid: 42, StartTime: &timestamppb.Timestamp{Seconds: 1764121200, Nanos: 123456789}},
		Elapsed: &durationpb.Duration{}, RootCpuTime: &durationpb.Duration{Seconds: 1, Nanos: 234000000}}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	const finishHex = "0a036a6f6212056772616e742a0f082a120b08f0b499c90610959aef3a32003a07080110809dca6f"
	if got := hex.EncodeToString(encoded); got != finishHex {
		t.Fatalf("encoding = %s, want %s", got, finishHex)
	}
	var decoded ReportFinishedRequest
	if err := proto.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(&decoded, report) || decoded.Elapsed == nil || decoded.Elapsed.AsDuration() != 0 {
		t.Fatalf("explicit zero runtime lost presence: %v", &decoded)
	}
}

func TestContractNames(t *testing.T) {
	if got := ErrorReason_JOB_NOT_WAITING.String(); got != "JOB_NOT_WAITING" {
		t.Errorf("ErrorReason name = %q", got)
	}
	if got := RunnerService_ServiceDesc.ServiceName; got != "tuna.RunnerService" {
		t.Errorf("RunnerService = %q", got)
	}
	if got := ControlService_ServiceDesc.ServiceName; got != "tuna.ControlService" {
		t.Errorf("ControlService = %q", got)
	}
}
