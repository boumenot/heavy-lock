package scheduler

import (
	"errors"

	"github.com/boumenot/heavy-lock/apps/tuna/contract"
	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"github.com/boumenot/heavy-lock/apps/tuna/storage"
	"google.golang.org/grpc/codes"
)

// ContractError maps scheduling and storage failures to structured broker
// errors. Unrecognized failures are normalized as internal errors.
func ContractError(err error) error {
	if err == nil {
		return nil
	}
	mapping := []struct {
		target error
		code   codes.Code
		reason pb.ErrorReason
	}{
		{ErrStreamClosed, codes.Canceled, pb.ErrorReason_REQUEST_CANCELLED},
		{ErrNotReady, codes.Unavailable, pb.ErrorReason_RECOVERING},
		{ErrStopped, codes.Unavailable, pb.ErrorReason_RECOVERING},
		{storage.ErrUnavailable, codes.Unavailable, pb.ErrorReason_STORAGE_UNAVAILABLE},
		{ErrWeightExceedsCapacity, codes.FailedPrecondition, pb.ErrorReason_WEIGHT_EXCEEDS_CAPACITY},
		{ErrNotWaiting, codes.FailedPrecondition, pb.ErrorReason_JOB_NOT_WAITING},
		{storage.ErrAdmissionBusy, codes.FailedPrecondition, pb.ErrorReason_ADMISSION_BUSY},
		{storage.ErrKeyReused, codes.AlreadyExists, pb.ErrorReason_IDEMPOTENCY_KEY_REUSED},
		{storage.ErrKeyDeleted, codes.FailedPrecondition, pb.ErrorReason_IDEMPOTENCY_KEY_DELETED},
		{storage.ErrRetryNotEligible, codes.FailedPrecondition, pb.ErrorReason_RETRY_NOT_ELIGIBLE},
		{storage.ErrNotFound, codes.NotFound, pb.ErrorReason_JOB_NOT_FOUND},
		{storage.ErrIdentity, codes.FailedPrecondition, pb.ErrorReason_CLAIM_MISMATCH},
		{storage.ErrTransition, codes.FailedPrecondition, pb.ErrorReason_JOB_NOT_WAITING},
	}
	for _, m := range mapping {
		if errors.Is(err, m.target) {
			return contract.Error(m.code, m.reason, err.Error(), nil)
		}
	}
	return contract.NormalizeError(err)
}
