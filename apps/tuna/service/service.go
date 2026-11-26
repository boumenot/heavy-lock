// Package service runs the per-user v2 broker: it owns the singleton lock,
// the database, the one scheduler and the loopback transport.
package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/boumenot/heavy-lock/apps/tuna/broker"
	"github.com/boumenot/heavy-lock/apps/tuna/scheduler"
	"github.com/boumenot/heavy-lock/apps/tuna/storage"
	"github.com/boumenot/heavy-lock/apps/tuna/transport"
)

// MaintenanceInterval paces grace expiry, release reconciliation retries and
// event retention.
const MaintenanceInterval = time.Second

// ErrLocked reports that another broker owns the singleton lock.
var ErrLocked = errors.New("another v2 broker holds the per-user singleton lock")

type Options struct {
	Port int
	// DataDir defaults to DefaultDataDir().
	DataDir string
	// Capacity 0 selects the v1-compatible default for this machine.
	Capacity       uint32
	ServiceVersion string
	Log            io.Writer
	// Sampler defaults to scheduler.DarwinSampler.
	Sampler scheduler.Sampler
	// EventMaxAge and EventMaxCount bound durable event replay; zero selects
	// the initial 24-hour/100,000-event limits. Job history is unaffected.
	EventMaxAge   time.Duration
	EventMaxCount int64
	// Ready, when set, is called once the transport listens and admission
	// is open.
	Ready func(port int)
}

// DefaultDataDir is the per-user broker directory. It is independent of the
// port, so brokers configured with different ports still exclude each other.
func DefaultDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Application Support", "heavy-lock", "v2"), nil
}

// Lock is the held singleton. The OS releases it when its owner exits; the
// lock file is never unlinked, so its inode stays stable.
type Lock struct{ file *os.File }

// AcquireLock takes a non-blocking exclusive flock on dir/broker.lock.
func AcquireLock(dir string) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create broker directory: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "broker.lock"), os.O_CREATE|os.O_RDWR|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open singleton lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock singleton: %w", err)
	}
	return &Lock{file: f}, nil
}

func (l *Lock) Release() error { return l.file.Close() }

// Run serves until ctx is cancelled or the transport fails, then closes the
// transport and records a clean scheduler stop. A listener conflict is
// returned (wrapping transport.ErrPortInUse); another port is never chosen.
func Run(ctx context.Context, o Options) (err error) {
	if runtime.GOOS != "darwin" {
		return errors.New("the v2 broker is implemented only on macOS")
	}
	logf := func(format string, args ...any) {
		if o.Log != nil {
			_, _ = fmt.Fprintf(o.Log, "tuna service: "+format+"\n", args...)
		}
	}
	if o.DataDir == "" {
		if o.DataDir, err = DefaultDataDir(); err != nil {
			return err
		}
	}
	if o.Sampler == nil {
		o.Sampler = scheduler.DarwinSampler{}
	}
	if o.Capacity == 0 {
		sample, err := o.Sampler.Sample(ctx)
		if err != nil {
			return fmt.Errorf("default capacity: %w", err)
		}
		o.Capacity = scheduler.DefaultCapacity(runtime.NumCPU(), sample.TotalMemoryBytes)
	}
	lock, err := AcquireLock(o.DataDir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Release()) }()

	store, err := storage.Open(ctx, filepath.Join(o.DataDir, "broker.db"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, store.Close()) }()

	hub := broker.NewHub()
	sched, _, err := scheduler.Start(ctx, store, scheduler.Options{
		Config:         scheduler.DefaultConfig(o.Capacity),
		ServiceVersion: o.ServiceVersion,
		PID:            int64(os.Getpid()),
		Sampler:        o.Sampler,
		Publish:        hub.Publish,
		Processes:      processProbe,
	})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, sched.Stop(context.WithoutCancel(ctx))) }()

	b, err := newBroker(sched, store, hub, o)
	if err != nil {
		return err
	}
	server, err := transport.Listen(o.Port, b)
	if err != nil {
		return err
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve() }()
	defer func() { err = errors.Join(err, server.Close()) }()

	// Earlier unreleased reservations are loaded as occupied capacity, so
	// opening admission cannot oversubscribe them.
	if _, err := sched.MarkReady(ctx); err != nil && !sched.Ready() {
		return fmt.Errorf("open admission: %w", err)
	}
	logf("ready on 127.0.0.1:%d with capacity %d", server.Port(), o.Capacity)
	if o.Ready != nil {
		o.Ready(server.Port())
	}

	var wg sync.WaitGroup
	maintenance, stop := context.WithCancel(ctx)
	defer func() { stop(); wg.Wait() }()
	wg.Add(1)
	go func() {
		defer wg.Done()
		maintain(maintenance, sched, b, logf)
	}()

	select {
	case <-ctx.Done():
		return nil
	case err := <-serveErr:
		if err == nil {
			err = errors.New("transport stopped")
		}
		return err
	}
}

func maintain(ctx context.Context, sched *scheduler.Scheduler, b *composite, logf func(string, ...any)) {
	ticker := time.NewTicker(MaintenanceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if _, err := sched.Tick(ctx); err != nil && ctx.Err() == nil {
			logf("grace maintenance: %v", err)
		}
		if _, err := sched.Reconcile(ctx); err != nil && ctx.Err() == nil {
			logf("release reconciliation: %v", err)
		}
		if err := b.maintain(ctx); err != nil && ctx.Err() == nil {
			logf("event retention: %v", err)
		}
	}
}
