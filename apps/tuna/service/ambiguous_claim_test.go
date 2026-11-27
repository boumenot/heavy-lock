package service

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
	"github.com/boumenot/tuna/apps/tuna/runner"
	"github.com/boumenot/tuna/apps/tuna/storage"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type capturedListener struct {
	net.Listener
	mu    sync.Mutex
	conns map[string]net.Conn
}

func (l *capturedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.mu.Lock()
		l.conns[conn.RemoteAddr().String()] = conn
		l.mu.Unlock()
	}
	return conn, err
}

func (l *capturedListener) closePeer(ctx context.Context) error {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return errors.New("claim has no network peer")
	}
	l.mu.Lock()
	conn := l.conns[p.Addr.String()]
	l.mu.Unlock()
	if conn == nil {
		return errors.New("claim connection was not captured")
	}
	return conn.Close()
}

type droppedClaimBroker struct {
	*composite
	listener *capturedListener
	claims   atomic.Int32
	dropped  chan error
}

type delayedClaimBroker struct {
	*composite
	commitFirst bool
	entered     chan struct{}
	release     chan struct{}
	returned    chan error
}

type delayedFinishBroker struct {
	*composite
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
}

func (b *delayedFinishBroker) ReportFinished(ctx context.Context, req *pb.ReportFinishedRequest) (*pb.Job, error) {
	b.enteredOnce.Do(func() { close(b.entered) })
	<-b.release
	return b.composite.ReportFinished(ctx, req)
}

func (b *delayedClaimBroker) ClaimLaunch(ctx context.Context, req *pb.ClaimLaunchRequest) (*pb.ClaimLaunchResponse, error) {
	var response *pb.ClaimLaunchResponse
	var err error
	if b.commitFirst {
		response, err = b.composite.ClaimLaunch(ctx, req)
		if err != nil {
			b.returned <- err
			return nil, err
		}
	}
	close(b.entered)
	<-b.release
	if !b.commitFirst {
		// Model an already-dispatched server operation that outlives the RPC.
		response, err = b.composite.ClaimLaunch(context.WithoutCancel(ctx), req)
	}
	b.returned <- err
	return response, err
}

func (b *droppedClaimBroker) ClaimLaunch(ctx context.Context, req *pb.ClaimLaunchRequest) (*pb.ClaimLaunchResponse, error) {
	response, err := b.composite.ClaimLaunch(ctx, req)
	if err != nil {
		return nil, err
	}
	if b.claims.Add(1) == 1 {
		b.dropped <- b.listener.closePeer(ctx)
		return nil, status.Error(codes.Unavailable, "claim response connection closed after commit")
	}
	return response, nil
}

func TestCommittedClaimSurvivesLostNetworkReply(t *testing.T) {
	b := startBroker(t, 2)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	captured := &capturedListener{Listener: listener, conns: map[string]net.Conn{}}
	proxy := &droppedClaimBroker{composite: b.domain, listener: captured, dropped: make(chan error, 1)}
	server := grpc.NewServer()
	pb.RegisterRunnerServiceServer(server, proxy)
	pb.RegisterControlServiceServer(server, proxy)
	go func() { _ = server.Serve(captured) }()
	t.Cleanup(server.Stop)

	file, count := counter(t)
	inv := b.start(t, func(o *runner.Options) { o.Port = listener.Addr().(*net.TCPAddr).Port },
		"/bin/sh", "-c", `printf x >> "$0"`, file)
	if code := inv.wait(t); code != 0 {
		t.Fatalf("runner exit %d after lost reply; log:\n%s", code, inv.log.String())
	}
	select {
	case err := <-proxy.dropped:
		if err != nil {
			t.Fatalf("did not drop the committed claim connection: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("claim response was not dropped")
	}
	if claims := proxy.claims.Load(); claims < 2 || count() != 1 {
		t.Fatalf("claim attempts = %d, command launches = %d; log:\n%s", claims, count(), inv.log.String())
	}
	b.waitState(t, inv.log.jobID(t), pb.JobState_JOB_STATE_FINISHED)
}

func TestCancelWhileClaimIsDelayed(t *testing.T) {
	for _, tc := range []struct {
		name        string
		commitFirst bool
	}{
		{name: "committed response", commitFirst: true},
		{name: "before commit", commitFirst: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := startBroker(t, 2)
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}

			proxy := &delayedClaimBroker{
				composite: b.domain, commitFirst: tc.commitFirst, entered: make(chan struct{}),
				release: make(chan struct{}), returned: make(chan error, 1),
			}
			server := grpc.NewServer()
			pb.RegisterRunnerServiceServer(server, proxy)
			pb.RegisterControlServiceServer(server, proxy)
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(server.Stop)

			file, count := counter(t)
			inv := b.start(t, func(o *runner.Options) { o.Port = listener.Addr().(*net.TCPAddr).Port },
				"/bin/sh", "-c", `printf x >> "$0"; exec /bin/sleep 30`, file)
			finished := false
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(proxy.release) }) }
			t.Cleanup(func() {
				release()
				if !finished {
					inv.wait(t)
				}
			})
			select {
			case <-proxy.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("claim did not reach the proxy")
			}
			id := inv.log.jobID(t)
			if _, err := b.sched.Cancel(context.Background(), id, nil); err != nil {
				t.Fatal(err)
			}
			select {
			case code := <-inv.done:
				finished = true
				if code != runner.ExitCancelled {
					t.Fatalf("cancelled before launch: exit %d; log:\n%s", code, inv.log.String())
				}
			case <-time.After(3 * time.Second):
				t.Fatal("cancellation waited for the delayed claim")
			}
			job := b.waitState(t, id, pb.JobState_JOB_STATE_FINISHED)
			if job.GetOutcome().GetKind() != pb.OutcomeKind_OUTCOME_KIND_CANCELLED_BEFORE_START ||
				(job.CancelAcknowledgedAt != nil) != tc.commitFirst {
				t.Fatalf("incorrect cancellation outcome: %v", job)
			}
			var reservation *storage.Reservation
			if err := b.store.View(context.Background(), func(r *storage.Reader) error {
				var err error
				reservation, err = r.Reservation(id)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			proof := "claim_prevented"
			if tc.commitFirst {
				proof = "runner_confirmed"
			}
			if (reservation.ClaimedAt != nil) != tc.commitFirst || reservation.FencedAt == nil ||
				reservation.ReleasedAt == nil || reservation.NoLaunchProof == nil || *reservation.NoLaunchProof != proof {
				t.Fatalf("incorrect no-launch evidence: %+v", reservation)
			}
			release()
			select {
			case err := <-proxy.returned:
				if tc.commitFirst && err != nil || !tc.commitFirst &&
					(status.Code(err) != codes.FailedPrecondition || contractReason(err) != pb.ErrorReason_GRANT_FENCED) {
					t.Fatalf("late claim outcome: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("delayed claim did not finish")
			}
			if count() != 0 {
				t.Fatal("command launched after cancellation")
			}
		})
	}
}

func TestCancelWhileFinishReportIsDelayed(t *testing.T) {
	b := startBroker(t, 2)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &delayedFinishBroker{composite: b.domain, entered: make(chan struct{}), release: make(chan struct{})}
	server := grpc.NewServer()
	pb.RegisterRunnerServiceServer(server, proxy)
	pb.RegisterControlServiceServer(server, proxy)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	inv := b.start(t, func(o *runner.Options) { o.Port = listener.Addr().(*net.TCPAddr).Port }, "/usr/bin/true")
	finished := false
	var once sync.Once
	release := func() { once.Do(func() { close(proxy.release) }) }
	t.Cleanup(func() {
		release()
		if !finished {
			inv.wait(t)
		}
	})
	select {
	case <-proxy.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("finish report did not reach the proxy")
	}
	id := inv.log.jobID(t)
	if _, err := b.sched.Cancel(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		job, err := b.sched.Job(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if job.CancelAcknowledgedAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancellation was not acknowledged while the finish report was delayed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	release()
	code := inv.wait(t)
	finished = true
	if code != 0 {
		t.Fatalf("runner exit %d; log:\n%s", code, inv.log.String())
	}
	job := b.waitState(t, id, pb.JobState_JOB_STATE_FINISHED)
	if job.GetOutcome().GetKind() != pb.OutcomeKind_OUTCOME_KIND_EXITED || job.GetOutcome().GetExitCode() != 0 {
		t.Fatalf("finish report lost the native outcome: %v", job)
	}
}
