package scheduler

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Config is fixed for one broker run; v2 has no live capacity change.
type Config struct {
	Capacity uint32
	// BackfillLimit (K) is broker-wide; 0 is strict FIFO.
	BackfillLimit        uint32
	MinFreeMemoryPercent float64
	MaxCPULoadPercent    float64
	// Settle is the minimum time since the previous admission before
	// sharing, so that the previous job's load appears in the next sample.
	Settle time.Duration
	// Grace is the queued-runner registration/disconnect grace period.
	Grace time.Duration
}

// DefaultCapacity is v1's min(processors/3, totalMemoryGiB/4), clamped to 2..8.
func DefaultCapacity(processors int, totalMemoryBytes uint64) uint32 {
	capacity := min(uint64(max(processors, 0)/3), totalMemoryBytes/(1<<30)/4)
	return uint32(min(max(capacity, 2), 8))
}

// DefaultConfig carries the design's initial, unmeasured defaults.
func DefaultConfig(capacity uint32) Config {
	return Config{
		Capacity:             capacity,
		BackfillLimit:        1,
		MinFreeMemoryPercent: 25,
		MaxCPULoadPercent:    50,
		Settle:               60 * time.Second,
		Grace:                30 * time.Second,
	}
}

func (c Config) validate() error {
	if c.Capacity == 0 {
		return errors.New("capacity must be positive")
	}
	if c.Grace <= 0 || c.Settle < 0 {
		return errors.New("grace must be positive and settle non-negative")
	}
	if c.MinFreeMemoryPercent < 0 || c.MinFreeMemoryPercent > 100 || c.MaxCPULoadPercent < 0 {
		return errors.New("invalid sharing thresholds")
	}
	return nil
}

// Sample is a point-in-time view of machine load.
type Sample struct {
	TotalMemoryBytes     uint64
	AvailableMemoryBytes uint64
	LoadAverage1m        float64
	LoadAverage5m        float64
	LoadAverage15m       float64
	// CPULoadPercent is runnable work relative to the processor count; it
	// may exceed 100.
	CPULoadPercent float64
}

func (s Sample) FreeMemoryPercent() float64 {
	return 100 * float64(s.AvailableMemoryBytes) / float64(s.TotalMemoryBytes)
}

func (s Sample) String() string {
	return fmt.Sprintf("%.0f%% memory free, CPU load %.0f%%", s.FreeMemoryPercent(), s.CPULoadPercent)
}

// Sampler reads live machine load. An error means the sample is unavailable,
// and work then waits to run alone.
type Sampler interface {
	Sample(context.Context) (Sample, error)
}

// SharingRefusal returns why a job that would not run alone must wait, or
// an empty string to allow sharing. It keeps v1's order and semantics.
func (c Config) SharingRefusal(sample *Sample, sinceLastAdmission time.Duration) string {
	if sinceLastAdmission < c.Settle {
		return "letting the previous admission settle"
	}
	if sample == nil {
		return "machine load unavailable; waiting to run alone"
	}
	if sample.FreeMemoryPercent() < c.MinFreeMemoryPercent || sample.CPULoadPercent > c.MaxCPULoadPercent {
		return "waiting for free resources (" + sample.String() + ")"
	}
	return ""
}

// DarwinSampler reads `sysctl -n hw.memsize vm.loadavg kern.memorystatus_level`,
// as v1 does on macOS.
type DarwinSampler struct{}

func (DarwinSampler) Sample(ctx context.Context) (Sample, error) {
	if runtime.GOOS != "darwin" {
		return Sample{}, errors.New("resource sampling is implemented only on macOS")
	}
	out, err := exec.CommandContext(ctx, "/usr/sbin/sysctl", "-n", "hw.memsize", "vm.loadavg", "kern.memorystatus_level").Output()
	if err != nil {
		return Sample{}, fmt.Errorf("sample resources: %w", err)
	}
	return ParseDarwinSample(string(out), runtime.NumCPU())
}

// ParseDarwinSample parses sysctl output. kern.memorystatus_level is the
// kernel's free-memory percentage, accounting for compressed memory.
func ParseDarwinSample(output string, processors int) (Sample, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 3 || processors <= 0 {
		return Sample{}, errors.New("unexpected sysctl output")
	}
	total, err := strconv.ParseUint(strings.TrimSpace(lines[0]), 10, 64)
	if err != nil || total == 0 {
		return Sample{}, errors.New("invalid hw.memsize")
	}
	fields := strings.Fields(strings.NewReplacer("{", " ", "}", " ").Replace(lines[1]))
	if len(fields) != 3 {
		return Sample{}, errors.New("invalid vm.loadavg")
	}
	var loads [3]float64
	for i, field := range fields {
		load, err := strconv.ParseFloat(field, 64)
		if err != nil || load < 0 || math.IsNaN(load) || math.IsInf(load, 0) {
			return Sample{}, errors.New("invalid vm.loadavg")
		}
		loads[i] = load
	}
	free, err := strconv.ParseUint(strings.TrimSpace(lines[2]), 10, 64)
	if err != nil || free > 100 {
		return Sample{}, errors.New("invalid kern.memorystatus_level")
	}
	return Sample{
		TotalMemoryBytes:     total,
		AvailableMemoryBytes: total * free / 100,
		LoadAverage1m:        loads[0],
		LoadAverage5m:        loads[1],
		LoadAverage15m:       loads[2],
		CPULoadPercent:       100 * loads[0] / float64(processors),
	}, nil
}
