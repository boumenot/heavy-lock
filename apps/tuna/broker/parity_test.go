package broker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/boumenot/heavy-lock/apps/tuna/cli"
	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"github.com/boumenot/heavy-lock/apps/tuna/scheduler"
	"github.com/boumenot/heavy-lock/apps/tuna/storage"
	"github.com/boumenot/heavy-lock/apps/tuna/transport"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type combined struct {
	*Control
	pb.UnimplementedRunnerServiceServer
}

func controlServer(t *testing.T, count int64) (*Control, *scheduler.Scheduler, pb.ControlServiceClient, int) {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	hub := NewHub()
	s, _, err := scheduler.Start(ctx, store, scheduler.Options{Config: scheduler.DefaultConfig(2), PID: 123, ServiceVersion: "test", Sampler: testSampler{}, Publish: hub.Publish})
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewControl(s, store, Options{ServiceVersion: "test", EventMaxCount: count, Hub: hub})
	if err != nil {
		t.Fatal(err)
	}
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()
	server, err := transport.Listen(port, &combined{Control: c})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Error(err)
		}
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	conn, err := grpc.NewClient(fmt.Sprintf("127.0.0.1:%d", port), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return c, s, pb.NewControlServiceClient(conn), port
}

func rest(t *testing.T, port int, method, path, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, data
}

func TestActualDomainGRPCRESTCLIParity(t *testing.T) {
	_, s, client, port := controlServer(t, 100000)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h, err := client.GetHealth(ctx, &pb.GetHealthRequest{})
	if err != nil || h.Ready {
		t.Fatalf("recovering health=%v %v", h, err)
	}
	if _, err := s.MarkReady(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PauseQueue(ctx, &pb.PauseQueueRequest{}); err != nil {
		t.Fatal(err)
	}
	code, data := rest(t, port, http.MethodGet, "/v1/status", "")
	var st pb.BrokerStatus
	if code != 200 || protojson.Unmarshal(data, &st) != nil || !st.Paused {
		t.Fatalf("REST status=%d %s", code, data)
	}
	var out, errors bytes.Buffer
	exit, handled := cli.Run(ctx, []string{"resume", "--port", fmt.Sprint(port), "--json"}, &out, &errors)
	if !handled || exit != 0 {
		t.Fatalf("CLI=%d %q", exit, errors.String())
	}
	statusAfter, err := client.GetStatus(ctx, &pb.GetStatusRequest{})
	if err != nil || statusAfter.Paused {
		t.Fatalf("shared scheduler status=%v %v", statusAfter, err)
	}
	missing := "00000000-0000-4000-8000-000000000001"
	_, grpcErr := client.GetJob(ctx, &pb.GetJobRequest{JobId: missing})
	code, data = rest(t, port, http.MethodGet, "/v1/jobs/"+missing, "")
	var restErr statuspb.Status
	if code != 404 || protojson.Unmarshal(data, &restErr) != nil || !proto.Equal(status.Convert(grpcErr).Proto(), &restErr) {
		t.Fatalf("errors differ: grpc=%v REST=%d %s", grpcErr, code, data)
	}
	out.Reset()
	errors.Reset()
	exit, _ = cli.Run(ctx, []string{"job", "--job", missing, "--port", fmt.Sprint(port)}, &out, &errors)
	if exit != 70 || out.Len() != 0 || !strings.Contains(errors.String(), "NotFound/JOB_NOT_FOUND") {
		t.Fatalf("CLI error=%d %q %q", exit, out.String(), errors.String())
	}
}

func TestSnapshotReplayPruningRequiresRefresh(t *testing.T) {
	c, s, client, _ := controlServer(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.MarkReady(ctx); err != nil {
		t.Fatal(err)
	}
	stream, err := client.WatchEvents(ctx, &pb.WatchEventsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := stream.Recv()
	if err != nil || snapshot.GetSnapshot() == nil || snapshot.EventSeq < 1 {
		t.Fatalf("snapshot=%v %v", snapshot, err)
	}
	if _, err := client.PauseQueue(ctx, &pb.PauseQueueRequest{}); err != nil {
		t.Fatal(err)
	}
	event, err := stream.Recv()
	if err != nil || event.EventSeq <= snapshot.EventSeq || event.GetStatusChanged() == nil || !event.GetStatusChanged().Paused {
		t.Fatalf("live event=%v %v", event, err)
	}
	if _, err := client.ResumeQueue(ctx, &pb.ResumeQueueRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	expired, err := client.WatchEvents(ctx, &pb.WatchEventsRequest{AfterEventSeq: snapshot.EventSeq})
	if err != nil {
		t.Fatal(err)
	}
	_, err = expired.Recv()
	if status.Code(err) != codes.OutOfRange {
		t.Fatalf("gap=%v", err)
	}
	refreshed, err := client.WatchEvents(ctx, &pb.WatchEventsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	newSnapshot, err := refreshed.Recv()
	if err != nil || newSnapshot.GetSnapshot() == nil || newSnapshot.GetSnapshot().Status.Paused {
		t.Fatalf("refresh=%v %v", newSnapshot, err)
	}
}

func TestForceStartPausedAndControlErrorParity(t *testing.T) {
	c, s, client, port := controlServer(t, 100000)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id := "00000000-0000-4000-8000-000000000001"
	req := &pb.ForceStartJobRequest{JobId: id}
	_, err := client.ForceStartJob(ctx, req)
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("recovery force-start=%v", err)
	}
	if _, err := s.MarkReady(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PauseQueue(ctx, &pb.PauseQueueRequest{}); err != nil {
		t.Fatal(err)
	}
	submit := &pb.SubmitJobRequest{IdempotencyKey: id, Command: "echo forced", Weight: 2,
		Runner: &pb.RunnerIncarnation{IncarnationId: "10000000-0000-4000-8000-000000000001",
			Process: &pb.ProcessIdentity{Pid: 123, StartTimeUnixNs: 1}}}
	result, _, err := s.Submit(ctx, submit)
	if err != nil {
		t.Fatal(err)
	}
	reg, _, _, err := s.Watch(ctx, result.Job.JobId, submit.Runner)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Disconnect(context.Background(), reg)
	code, data := rest(t, port, http.MethodPost, "/v1/jobs/"+result.Job.JobId+"/force-start", "{}")
	var forced pb.Job
	if code != 200 || protojson.Unmarshal(data, &forced) != nil || !forced.Forced || forced.State != pb.JobState_JOB_STATE_RESERVED {
		t.Fatalf("forced=%d %s", code, data)
	}
	st, err := client.GetStatus(ctx, &pb.GetStatusRequest{})
	if err != nil || !st.Paused || st.Occupied != 2 {
		t.Fatalf("forced occupancy=%v %v", st, err)
	}
	var out, stderr bytes.Buffer
	exit, _ := cli.Run(ctx, []string{"job", "--job", forced.JobId, "--port", fmt.Sprint(port)}, &out, &stderr)
	if exit != 0 || !strings.Contains(out.String(), "FORCED") {
		t.Fatalf("forced CLI=%d %q %q", exit, out.String(), stderr.String())
	}
	missing := "00000000-0000-4000-8000-000000000002"
	for _, test := range []struct {
		name, method, path, body string
		call                     func() error
	}{
		{"move", http.MethodPost, "/move", `{"toBack":true}`, func() error {
			_, err := client.MoveJob(ctx, &pb.MoveJobRequest{JobId: missing, Target: &pb.MoveJobRequest_ToBack{ToBack: true}})
			return err
		}},
		{"force", http.MethodPost, "/force-start", "{}", func() error {
			_, err := client.ForceStartJob(ctx, &pb.ForceStartJobRequest{JobId: missing})
			return err
		}},
		{"delete", http.MethodDelete, "", "", func() error {
			_, err := client.DeleteJob(ctx, &pb.DeleteJobRequest{JobId: missing})
			return err
		}},
		{"cancel", http.MethodPost, "/cancel", "{}", func() error {
			_, err := client.CancelJob(ctx, &pb.CancelJobRequest{JobId: missing})
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			grpcErr := test.call()
			code, data := rest(t, port, test.method, "/v1/jobs/"+missing+test.path, test.body)
			var restErr statuspb.Status
			if code != 404 || protojson.Unmarshal(data, &restErr) != nil || !proto.Equal(status.Convert(grpcErr).Proto(), &restErr) {
				t.Fatalf("grpc=%v REST=%d %s", grpcErr, code, data)
			}
		})
	}
}
func TestReorderAndPaginationUseDomain(t *testing.T) {
	c, s, client, port := controlServer(t, 100000)
	ctx := context.Background()
	if _, err := s.MarkReady(ctx); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := 1; i <= 3; i++ {
		req := &pb.SubmitJobRequest{IdempotencyKey: fmt.Sprintf("00000000-0000-4000-8000-%012d", i),
			Command: fmt.Sprintf("command%d", i), Weight: 1, Runner: &pb.RunnerIncarnation{
				IncarnationId: fmt.Sprintf("10000000-0000-4000-8000-%012d", i),
				Process:       &pb.ProcessIdentity{Pid: int64(i), StartTimeUnixNs: 1}}}
		result, _, err := s.Submit(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, result.Job.JobId)
	}
	q, err := c.MoveJob(ctx, &pb.MoveJobRequest{JobId: ids[0], Target: &pb.MoveJobRequest_AfterJobId{AfterJobId: ids[1]}})
	if err != nil || q.Jobs[1].JobId != ids[0] {
		t.Fatalf("after=%v %v", q, err)
	}
	var out, stderr bytes.Buffer
	exit, _ := cli.Run(ctx, []string{"reorder", "--job", ids[2], "--front", "--port", fmt.Sprint(port)}, &out, &stderr)
	if exit != 0 {
		t.Fatalf("CLI reorder=%d %q", exit, stderr.String())
	}
	q, err = client.GetQueue(ctx, &pb.GetQueueRequest{})
	if err != nil || q.Jobs[0].JobId != ids[2] {
		t.Fatalf("CLI reorder not reflected=%v %v", q, err)
	}
	code, data := rest(t, port, http.MethodPost, "/v1/jobs/"+ids[2]+"/move", `{"toBack":true}`)
	if code != 200 {
		t.Fatalf("REST reorder=%d %s", code, data)
	}
	q, err = client.GetQueue(ctx, &pb.GetQueueRequest{})
	if err != nil || q.Jobs[2].JobId != ids[2] {
		t.Fatalf("REST reorder not reflected=%v %v", q, err)
	}
	first, err := c.ListJobs(ctx, &pb.ListJobsRequest{PageSize: 2})
	if err != nil || len(first.Jobs) != 2 || first.NextPageToken == "" {
		t.Fatalf("page1=%v %v", first, err)
	}
	last, err := c.ListJobs(ctx, &pb.ListJobsRequest{PageSize: 2, PageToken: first.NextPageToken})
	if err != nil || len(last.Jobs) != 1 || last.Jobs[0].JobId == first.Jobs[0].JobId {
		t.Fatalf("page2=%v %v", last, err)
	}
	_, err = c.ListJobs(ctx, &pb.ListJobsRequest{PageToken: first.NextPageToken, ClientName: proto.String("changed")})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("changed filter=%v", err)
	}
	_, err = c.DeleteJob(ctx, &pb.DeleteJobRequest{JobId: ids[0]})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unsafe deletion=%v", err)
	}
	job, err := c.CancelJob(ctx, &pb.CancelJobRequest{JobId: ids[0], Reason: proto.String("test cancel")})
	if err != nil || job.State != pb.JobState_JOB_STATE_FINISHED || job.GetCancelReason() != "test cancel" {
		t.Fatalf("queued cancel=%v %v", job, err)
	}
	if err := c.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	if retained, err := c.GetJob(ctx, &pb.GetJobRequest{JobId: ids[0]}); err != nil || retained.State != pb.JobState_JOB_STATE_FINISHED {
		t.Fatalf("pruning changed history=%v %v", retained, err)
	}
	if _, err := c.DeleteJob(ctx, &pb.DeleteJobRequest{JobId: ids[0]}); err != nil {
		t.Fatal(err)
	}
	_, err = c.GetSubmission(ctx, &pb.GetSubmissionRequest{IdempotencyKey: "00000000-0000-4000-8000-000000000001"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("deleted key=%v", err)
	}
	out.Reset()
	stderr.Reset()
	exit, _ = cli.Run(ctx, []string{"cancel", "--job", ids[1], "--port", fmt.Sprint(port)}, &out, &stderr)
	if exit != 0 {
		t.Fatalf("CLI cancel=%d %q", exit, stderr.String())
	}
	code, data = rest(t, port, http.MethodGet, "/v1/jobs/"+ids[1], "")
	var cancelled pb.Job
	if code != 200 || protojson.Unmarshal(data, &cancelled) != nil || cancelled.State != pb.JobState_JOB_STATE_FINISHED {
		t.Fatalf("cancel not reflected=%d %s", code, data)
	}
}

type eventStream struct {
	ctx  context.Context
	send func(*pb.BrokerEvent) error
}

func (s *eventStream) Send(event *pb.BrokerEvent) error { return s.send(event) }
func (s *eventStream) Context() context.Context         { return s.ctx }
func (*eventStream) SetHeader(metadata.MD) error        { return nil }
func (*eventStream) SendHeader(metadata.MD) error       { return nil }
func (*eventStream) SetTrailer(metadata.MD)             {}
func (*eventStream) SendMsg(any) error                  { return nil }
func (*eventStream) RecvMsg(any) error                  { return io.EOF }

func TestSlowWatchRequiresRefreshWithoutBlockingPublisher(t *testing.T) {
	c, s, _, _ := controlServer(t, 100000)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.MarkReady(ctx); err != nil {
		t.Fatal(err)
	}
	stream := &eventStream{ctx: ctx, send: func(snapshot *pb.BrokerEvent) error {
		if snapshot.GetSnapshot() == nil {
			t.Fatal("expected initial snapshot")
		}
		events := make([]*pb.BrokerEvent, 501)
		for i := range events {
			events[i] = &pb.BrokerEvent{EventSeq: snapshot.EventSeq + int64(i) + 1}
		}
		c.opts.Hub.Publish(events)
		return nil
	}}
	err := c.WatchEvents(&pb.WatchEventsRequest{}, stream)
	if status.Code(err) != codes.OutOfRange {
		t.Fatalf("overflow=%v", err)
	}
}
