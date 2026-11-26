package scheduler

import (
	"strings"
	"testing"
	"time"
)

func TestParseDarwinSample(t *testing.T) {
	s, err := ParseDarwinSample("34359738368\n{ 2.50 2.00 1.50 }\n40\n", 10)
	if err != nil {
		t.Fatal(err)
	}
	if s.TotalMemoryBytes != 34359738368 || s.AvailableMemoryBytes != 34359738368*40/100 ||
		s.LoadAverage1m != 2.5 || s.CPULoadPercent != 25 {
		t.Fatalf("sample = %+v", s)
	}
	for _, bad := range []string{"", "0\n{ 1 }\n40", "1\n{ x }\n40", "1\n{ 1 }\n101", "1\n{ 1 }"} {
		if _, err := ParseDarwinSample(bad, 10); err == nil {
			t.Errorf("parsed %q", bad)
		}
	}
}

func TestSharingRefusalKeepsV1OrderAndThresholds(t *testing.T) {
	c := DefaultConfig(4)
	ok := &Sample{TotalMemoryBytes: 100, AvailableMemoryBytes: 25, CPULoadPercent: 50}
	if r := c.SharingRefusal(ok, time.Minute); r != "" {
		t.Fatalf("boundary sample refused: %s", r)
	}
	if r := c.SharingRefusal(ok, time.Minute-time.Nanosecond); !strings.Contains(r, "settle") {
		t.Fatalf("settle not first: %q", r)
	}
	if r := c.SharingRefusal(nil, time.Minute); !strings.Contains(r, "unavailable") {
		t.Fatalf("missing sample allowed: %q", r)
	}
	for _, s := range []*Sample{{TotalMemoryBytes: 100, AvailableMemoryBytes: 24}, {TotalMemoryBytes: 100, AvailableMemoryBytes: 90, CPULoadPercent: 51}} {
		if r := c.SharingRefusal(s, time.Minute); !strings.Contains(r, "free resources") {
			t.Fatalf("%+v allowed: %q", s, r)
		}
	}
}

func TestDefaultCapacityMatchesV1(t *testing.T) {
	for _, c := range []struct {
		cpus int
		mem  uint64
		want uint32
	}{{4, 8 << 30, 2}, {12, 32 << 30, 4}, {30, 24 << 30, 6}, {64, 512 << 30, 8}} {
		if got := DefaultCapacity(c.cpus, c.mem); got != c.want {
			t.Errorf("DefaultCapacity(%d, %d) = %d, want %d", c.cpus, c.mem, got, c.want)
		}
	}
}
