package storage

import (
	"errors"
	"fmt"

	"github.com/boumenot/heavy-lock/apps/tuna/contract"
	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func optionalTimestamp(nanoseconds *int64) *timestamppb.Timestamp {
	if nanoseconds == nil {
		return nil
	}
	return contract.Timestamp(*nanoseconds)
}

func (r *Reader) storedDuration(milliseconds *int64) (*durationpb.Duration, error) {
	if milliseconds == nil {
		return nil, nil
	}
	duration := contract.MillisecondDuration(*milliseconds)
	err := duration.CheckValid()
	if *milliseconds < 0 {
		err = errors.Join(err, errors.New("negative duration"))
	}
	if err != nil {
		err = fmt.Errorf("invalid stored duration: %w", err)
		r.err = errors.Join(r.err, err)
		return nil, err
	}
	return duration, nil
}

func processStart(process *pb.ProcessIdentity) (int64, error) {
	if process == nil || process.Pid <= 0 {
		return 0, ErrIdentity
	}
	start, err := contract.UnixNano(process.StartTime)
	if err != nil || start <= 0 {
		return 0, errors.Join(ErrIdentity, err)
	}
	return start, nil
}
