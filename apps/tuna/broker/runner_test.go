package broker

import (
	"context"
	"testing"

	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestReportFinishedRejectsNegativeMeasurements(t *testing.T) {
	for _, field := range []string{"elapsed", "root CPU"} {
		t.Run(field, func(t *testing.T) {
			request := &pb.ReportFinishedRequest{JobId: "job", GrantId: "grant",
				Runner:  &pb.RunnerIncarnation{IncarnationId: "1b4e28ba-2fa1-41d2-883f-0016d3cca427", Process: &pb.ProcessIdentity{Pid: 1, StartTimeUnixNs: 1}},
				Process: &pb.ProcessIdentity{Pid: 2, StartTimeUnixNs: 2},
				Outcome: &pb.Outcome{Kind: pb.OutcomeKind_OUTCOME_KIND_EXITED, ExitCode: proto.Int64(0)}}
			if field == "elapsed" {
				request.ElapsedMs = proto.Int64(-1)
			} else {
				request.RootCpuTimeMs = proto.Int64(-1)
			}
			if _, err := (&Runner{}).ReportFinished(context.Background(), request); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("negative %s returned %v", field, err)
			}
		})
	}
}
