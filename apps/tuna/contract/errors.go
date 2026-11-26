package contract

import (
	"context"
	"errors"
	"fmt"

	heavylockv2 "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func Error(code codes.Code, reason heavylockv2.ErrorReason, message string, metadata map[string]string) error {
	if code == codes.OK || reason == heavylockv2.ErrorReason_ERROR_REASON_UNSPECIFIED {
		return fmt.Errorf("invalid broker error code %v or reason %v", code, reason)
	}
	if _, ok := heavylockv2.ErrorReason_name[int32(reason)]; !ok {
		return fmt.Errorf("unknown broker error reason %d", reason)
	}
	result, err := status.New(code, message).WithDetails(&errdetails.ErrorInfo{
		Reason:   reason.String(),
		Domain:   ServiceIdentity,
		Metadata: metadata,
	})
	if err != nil {
		return fmt.Errorf("construct broker error details: %w", err)
	}
	return result.Err()
}

func NormalizeError(err error) error {
	if err == nil {
		return nil
	}
	result := status.Convert(err)
	for _, detail := range result.Details() {
		info, ok := detail.(*errdetails.ErrorInfo)
		if !ok || info.Domain != ServiceIdentity {
			continue
		}
		if reason, ok := heavylockv2.ErrorReason_value[info.Reason]; ok && reason != 0 {
			return result.Err()
		}
	}
	if errors.Is(err, context.Canceled) {
		return Error(codes.Canceled, heavylockv2.ErrorReason_REQUEST_CANCELLED, err.Error(), nil)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Error(codes.DeadlineExceeded, heavylockv2.ErrorReason_DEADLINE_EXCEEDED, err.Error(), nil)
	}
	switch result.Code() {
	case codes.Unimplemented:
		return Error(codes.Unimplemented, heavylockv2.ErrorReason_OPERATION_NOT_IMPLEMENTED, result.Message(), nil)
	case codes.Canceled:
		return Error(codes.Canceled, heavylockv2.ErrorReason_REQUEST_CANCELLED, result.Message(), nil)
	case codes.DeadlineExceeded:
		return Error(codes.DeadlineExceeded, heavylockv2.ErrorReason_DEADLINE_EXCEEDED, result.Message(), nil)
	default:
		return Error(codes.Internal, heavylockv2.ErrorReason_INTERNAL_ERROR,
			"broker returned an unstructured error: "+err.Error(), nil)
	}
}
