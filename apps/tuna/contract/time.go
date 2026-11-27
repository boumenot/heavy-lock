package contract

import (
	"errors"
	"math"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Timestamp preserves every bit of a stored Unix-nanosecond value.
func Timestamp(nanoseconds int64) *timestamppb.Timestamp {
	return timestamppb.New(time.Unix(0, nanoseconds))
}

// UnixNano rejects protobuf timestamps outside SQLite's int64 nanosecond range.
func UnixNano(timestamp *timestamppb.Timestamp) (int64, error) {
	if timestamp == nil {
		return 0, errors.New("timestamp is required")
	}
	if err := timestamp.CheckValid(); err != nil {
		return 0, err
	}
	const billion = int64(time.Second)
	const minSeconds = int64(math.MinInt64)/billion - 1
	const minNanos = billion + int64(math.MinInt64)%billion
	const maxSeconds = int64(math.MaxInt64) / billion
	const maxNanos = int64(math.MaxInt64) % billion
	seconds, nanos := timestamp.Seconds, int64(timestamp.Nanos)
	if seconds < minSeconds || seconds > maxSeconds ||
		seconds == minSeconds && nanos < minNanos ||
		seconds == maxSeconds && nanos > maxNanos {
		return 0, errors.New("timestamp is outside the int64 Unix-nanosecond range")
	}
	return time.Unix(seconds, nanos).UnixNano(), nil
}

// MillisecondDuration retains the precision of the existing duration columns.
func MillisecondDuration(milliseconds int64) *durationpb.Duration {
	return &durationpb.Duration{
		Seconds: milliseconds / 1000,
		Nanos:   int32(milliseconds%1000) * int32(time.Millisecond),
	}
}

// Milliseconds rejects invalid or sub-millisecond values rather than rounding.
func Milliseconds(duration *durationpb.Duration) (int64, error) {
	if duration == nil {
		return 0, errors.New("duration is required")
	}
	if err := duration.CheckValid(); err != nil {
		return 0, err
	}
	if duration.Nanos%int32(time.Millisecond) != 0 {
		return 0, errors.New("duration must have millisecond precision")
	}
	return duration.Seconds*1000 + int64(duration.Nanos)/int64(time.Millisecond), nil
}

func OptionalMilliseconds(duration *durationpb.Duration) (*int64, error) {
	if duration == nil {
		return nil, nil
	}
	milliseconds, err := Milliseconds(duration)
	if err != nil || milliseconds < 0 {
		return nil, errors.Join(errors.New("duration must be nonnegative and have millisecond precision"), err)
	}
	return &milliseconds, nil
}
