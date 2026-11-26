package heavylockv2

import (
	"encoding/hex"
	"testing"

	"google.golang.org/protobuf/proto"
)

// submitJobHex is the wire encoding of sampleSubmitJob. The Swift bindings
// test in packages/heavylock-v2-swift asserts the same bytes.
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

func TestContractNames(t *testing.T) {
	if got := ErrorReason_JOB_NOT_WAITING.String(); got != "JOB_NOT_WAITING" {
		t.Errorf("ErrorReason name = %q", got)
	}
	if got := RunnerService_ServiceDesc.ServiceName; got != "heavylock.v2.RunnerService" {
		t.Errorf("RunnerService = %q", got)
	}
	if got := ControlService_ServiceDesc.ServiceName; got != "heavylock.v2.ControlService" {
		t.Errorf("ControlService = %q", got)
	}
}
