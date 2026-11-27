package contract

import (
	"context"
	"errors"
	"testing"

	tunapb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestNormalizeErrorPreservesContractAndReportsFailures(t *testing.T) {
	structured := Error(codes.FailedPrecondition, tunapb.ErrorReason_JOB_NOT_WAITING, "not waiting", nil)
	if !proto.Equal(status.Convert(NormalizeError(structured)).Proto(), status.Convert(structured).Proto()) {
		t.Fatal("a structured broker error must be preserved")
	}
	tests := []struct {
		name   string
		err    error
		code   codes.Code
		reason tunapb.ErrorReason
	}{
		{"cancelled", context.Canceled, codes.Canceled, tunapb.ErrorReason_REQUEST_CANCELLED},
		{"deadline", context.DeadlineExceeded, codes.DeadlineExceeded, tunapb.ErrorReason_DEADLINE_EXCEEDED},
		{"unimplemented", status.Error(codes.Unimplemented, "not implemented"), codes.Unimplemented, tunapb.ErrorReason_OPERATION_NOT_IMPLEMENTED},
		{"unstructured", errors.New("storage failed"), codes.Internal, tunapb.ErrorReason_INTERNAL_ERROR},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := status.Convert(NormalizeError(tt.err))
			if got.Code() != tt.code {
				t.Fatalf("code = %v, want %v", got.Code(), tt.code)
			}
			details := got.Details()
			if len(details) != 1 {
				t.Fatalf("details = %v", details)
			}
			info, ok := details[0].(*errdetails.ErrorInfo)
			if !ok || info.Domain != ServiceIdentity || info.Reason != tt.reason.String() {
				t.Fatalf("detail = %v", details[0])
			}
		})
	}
}
