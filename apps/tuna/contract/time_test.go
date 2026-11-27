package contract

import (
	"math"
	"testing"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestTimestampPreservesUnixNanoseconds(t *testing.T) {
	for _, want := range []int64{math.MinInt64, -1, 0, 1, 1764121200123456789, math.MaxInt64} {
		got, err := UnixNano(Timestamp(want))
		if err != nil || got != want {
			t.Errorf("round trip %d = %d, %v", want, got, err)
		}
	}
	for _, invalid := range []*timestamppb.Timestamp{
		nil, {Seconds: 9223372036, Nanos: 854775808}, {Seconds: -9223372037, Nanos: 145224191},
		{Seconds: 0, Nanos: -1},
	} {
		if _, err := UnixNano(invalid); err == nil {
			t.Errorf("accepted out-of-range timestamp %v", invalid)
		}
	}
}

func TestDurationMillisecondPrecision(t *testing.T) {
	for _, want := range []int64{-1234, 0, 1, 1234} {
		got, err := Milliseconds(MillisecondDuration(want))
		if err != nil || got != want {
			t.Errorf("round trip %d = %d, %v", want, got, err)
		}
	}
	for _, invalid := range []*durationpb.Duration{
		nil, {Nanos: 1}, {Seconds: 1, Nanos: -1}, {Seconds: 315576000001},
	} {
		if _, err := Milliseconds(invalid); err == nil {
			t.Errorf("accepted invalid or sub-millisecond duration %v", invalid)
		}
	}
}
