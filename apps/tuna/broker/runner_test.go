package broker

import (
	"context"
	"testing"

	"github.com/boumenot/tuna/apps/tuna/contract"
	pb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestReportFinishedRejectsInvalidMeasurements(t *testing.T) {
	for _, field := range []string{"elapsed", "root CPU"} {
		for _, invalid := range []*durationpb.Duration{
			contract.MillisecondDuration(-1), {Nanos: 1}, {Seconds: 1, Nanos: -1},
		} {
			request := &pb.ReportFinishedRequest{JobId: "job", GrantId: "grant",
				Runner:  &pb.RunnerIncarnation{IncarnationId: "1b4e28ba-2fa1-41d2-883f-0016d3cca427", Process: &pb.ProcessIdentity{Pid: 1, StartTime: contract.Timestamp(1)}},
				Process: &pb.ProcessIdentity{Pid: 2, StartTime: contract.Timestamp(2)},
				Outcome: &pb.Outcome{Kind: pb.OutcomeKind_OUTCOME_KIND_EXITED, ExitCode: proto.Int64(0)}}
			if field == "elapsed" {
				request.Elapsed = invalid
			} else {
				request.RootCpuTime = invalid
			}
			if _, err := (&Runner{}).ReportFinished(context.Background(), request); status.Code(err) != codes.InvalidArgument {
				t.Errorf("invalid %s %v returned %v", field, invalid, err)
			}
		}
	}
}

func TestSubmitRejectsOutOfRangeProcessTimestamp(t *testing.T) {
	req := &pb.SubmitJobRequest{
		IdempotencyKey: "1b4e28ba-2fa1-41d2-883f-0016d3cca427", Command: "job", Weight: 1,
		Runner: &pb.RunnerIncarnation{IncarnationId: "1b4e28ba-2fa1-41d2-883f-0016d3cca427",
			Process: &pb.ProcessIdentity{Pid: 1, StartTime: &timestamppb.Timestamp{Seconds: 9223372036, Nanos: 854775808}}},
	}
	if _, err := (&Runner{}).SubmitJob(context.Background(), req); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("out-of-range process timestamp returned %v", err)
	}
}
