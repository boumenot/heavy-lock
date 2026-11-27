// Package service runs the per-user v2 broker: it owns the singleton lock,
// the database, the one scheduler and the loopback transport.
package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/boumenot/heavy-lock/apps/tuna/broker"
	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"github.com/boumenot/heavy-lock/apps/tuna/jobmetrics"
	"github.com/boumenot/heavy-lock/apps/tuna/procid"
	"github.com/boumenot/heavy-lock/apps/tuna/scheduler"
	"github.com/boumenot/heavy-lock/apps/tuna/storage"
	"github.com/boumenot/heavy-lock/apps/tuna/transport"
	"golang.org/x/sys/unix"
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
	// LockDir is for isolated embedding and tests. The CLI always uses the
	// account's stable default directory, independent of DataDir and HOME.
	LockDir string
	// Capacity 0 selects the v1-compatible default for this machine.
	Capacity       uint32
	ServiceVersion string
	Log            io.Writer
	// Sampler defaults to scheduler.DarwinSampler.
	Sampler scheduler.Sampler
	// JobSampler observes historical job resources independently of admission.
	JobSampler jobmetrics.Source
	// EventMaxAge and EventMaxCount bound durable event replay; zero selects
	// the initial 24-hour/100,000-event limits. Job history is unaffected.
	EventMaxAge   time.Duration
	EventMaxCount int64
	// LaunchTimeout defaults to scheduler.LaunchTimeout.
	LaunchTimeout time.Duration
	// Cancellation timeouts default to the scheduler's initial deadlines.
	CancelAckTimeout        time.Duration
	CancelCompletionTimeout time.Duration
	// Ready, when set, is called once the transport listens and admission
	// is open.
	Ready func(port int)
}

// DefaultDataDir uses the account database, not the caller's HOME environment.
func DefaultDataDir() (string, error) {
	if os.Getuid() != os.Geteuid() {
		return "", errors.New("broker does not support differing real and effective users")
	}
	account, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolve user account: %w", err)
	}
	if !filepath.IsAbs(account.HomeDir) {
		return "", errors.New("user account has no absolute home directory")
	}
	return filepath.Join(account.HomeDir, "Library", "Application Support", "heavy-lock", "v2"), nil
}

// Lock is the held singleton. The OS releases it when its owner exits; the
// lock file is never unlinked, so its inode stays stable.
type Lock struct{ file *os.File }

// AcquireLock takes a non-blocking exclusive flock on dir/broker.lock.
func AcquireLock(dir string) (*Lock, error) {
	if err := privateDir(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "broker.lock")
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open singleton lock: %w", err)
	}
	f := os.NewFile(uintptr(fd), path)
	if err := privateFileInfo(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("singleton lock: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock singleton: %w", err)
	}
	return &Lock{file: f}, nil
}

func (l *Lock) Release() error { return l.file.Close() }

func privateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create broker directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect broker directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(info) {
		return fmt.Errorf("broker directory %q must be owned by the current user and private (0700); mode is %04o", dir, info.Mode().Perm())
	}
	return rejectACL(dir)
}

// ls -e prints ACL entries on separate lines; -q prevents file names with
// newlines from being confused with entries. Reject all ACLs conservatively.
func rejectACL(path string) error {
	cmd := exec.Command("/bin/ls", "-ldeq", "--", path)
	cmd.Env = []string{"LC_ALL=C"}
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("inspect ACL of %q: %w", path, err)
	}
	if strings.Count(string(output), "\n") != 1 {
		return fmt.Errorf("%q must not have an access control list", path)
	}
	return nil
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func privateFileInfo(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(info) {
		return fmt.Errorf("%q must be a private regular file owned by the current user", f.Name())
	}
	return rejectACL(f.Name())
}

func privateStorageFiles(dir string) error {
	for _, name := range []string{"broker.db", "broker.db-wal", "broker.db-shm"} {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect storage file %q: %w", path, err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(info) {
			return fmt.Errorf("storage file %q must be a private regular file owned by the current user", path)
		}
		if err := rejectACL(path); err != nil {
			return err
		}
	}
	return nil
}

// Run serves until ctx is cancelled or the transport fails, then closes the
// transport and records a clean scheduler stop. A listener conflict is
// returned (wrapping transport.ErrPortInUse); another port is never chosen.
// Reservations left by an earlier run stay held as occupied capacity until
// reconciliation establishes the required release evidence.
func Run(ctx context.Context, o Options) (err error) {
	if runtime.GOOS != "darwin" {
		return errors.New("the v2 broker is implemented only on macOS")
	}
	var logMu sync.Mutex
	logf := func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		if o.Log != nil {
			_, _ = fmt.Fprintf(o.Log, "tuna service: "+format+"\n", args...)
		}
	}
	if o.LockDir == "" {
		if o.LockDir, err = DefaultDataDir(); err != nil {
			return err
		}
	}
	if o.DataDir == "" {
		o.DataDir = o.LockDir
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
	lock, err := AcquireLock(o.LockDir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Release()) }()

	if err := privateDir(o.DataDir); err != nil {
		return err
	}
	if err := privateStorageFiles(o.DataDir); err != nil {
		return err
	}
	store, err := storage.Open(ctx, filepath.Join(o.DataDir, "broker.db"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, store.Close()) }()

	hub := broker.NewHub()
	cfg := scheduler.DefaultConfig(o.Capacity)
	if o.LaunchTimeout != 0 {
		cfg.LaunchTimeout = o.LaunchTimeout
	}
	if o.CancelAckTimeout != 0 {
		cfg.CancelAckTimeout = o.CancelAckTimeout
	}
	if o.CancelCompletionTimeout != 0 {
		cfg.CancelCompletionTimeout = o.CancelCompletionTimeout
	}
	sched, _, err := scheduler.Start(ctx, store, scheduler.Options{
		Config:         cfg,
		ServiceVersion: o.ServiceVersion,
		PID:            int64(os.Getpid()),
		Sampler:        o.Sampler,
		Publish:        hub.Publish,
		Processes:      procid.Probe{},
		Terminator:     procid.Terminator{},
		Warnf:          logf,
	})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, sched.Stop(context.WithoutCancel(ctx))) }()

	b, err := newBroker(sched, store, hub, o)
	if err != nil {
		return err
	}
	if err := b.resources.restore(ctx); err != nil {
		return fmt.Errorf("restore job resource summaries: %w", err)
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
	wg.Add(2)
	go func() {
		defer wg.Done()
		maintain(maintenance, sched, b, logf)
	}()
	go func() {
		defer wg.Done()
		b.resources.run(maintenance, logf)
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
	warned := map[string]bool{}
	warnTakeovers := func() {
		snapshot, err := sched.Snapshot(ctx)
		if err != nil {
			if ctx.Err() == nil {
				logf("inspect pending takeovers: %v", err)
			}
			return
		}
		active := map[string]bool{}
		for _, j := range snapshot.ActiveJobs {
			if j.BrokerTermination == nil || j.State == pb.JobState_JOB_STATE_FINISHED {
				continue
			}
			active[j.JobId] = true
			if !warned[j.JobId] {
				logf("job %s: takeover %s; force-stopping tracked processes with fingerprint-checked signals, reservation held until exit is verified",
					j.JobId, j.BrokerTermination.Reason)
			}
		}
		warned = active
	}
	warnTakeovers()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if _, err := sched.Tick(ctx); err != nil && ctx.Err() == nil {
			logf("grace maintenance: %v", err)
		}
		discovery, cancel := context.WithTimeout(ctx, jobSampleTimeout)
		err := sched.Discover(discovery, b.discovery)
		cancel()
		if err != nil && ctx.Err() == nil {
			logf("process discovery: %v", err)
		}
		if _, err := sched.Reconcile(ctx); err != nil && ctx.Err() == nil {
			logf("release reconciliation: %v", err)
		}
		warnTakeovers()
		if err := sched.Terminate(ctx); err != nil && ctx.Err() == nil {
			logf("takeover termination: %v", err)
		}
		if err := b.maintain(ctx); err != nil && ctx.Err() == nil {
			logf("event retention: %v", err)
		}
	}
}
