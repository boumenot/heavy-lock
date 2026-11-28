package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/boumenot/tuna/apps/tuna/contract"
	pb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"
)

type sseBroker struct {
	fakeBroker
	watch func(*pb.WatchEventsRequest, grpc.ServerStreamingServer[pb.BrokerEvent]) error
}

func (b *sseBroker) GetHealth(context.Context, *pb.GetHealthRequest) (*pb.Health, error) {
	return &pb.Health{Live: true, StartedAt: contract.Timestamp(123)}, nil
}

func (b *sseBroker) WatchEvents(req *pb.WatchEventsRequest, stream grpc.ServerStreamingServer[pb.BrokerEvent]) error {
	return b.watch(req, stream)
}

type deadlineRecorder struct{ *httptest.ResponseRecorder }

func (*deadlineRecorder) SetWriteDeadline(time.Time) error { return nil }

func TestSSESnapshotAndResume(t *testing.T) {
	var cursors []int64
	broker := &sseBroker{watch: func(req *pb.WatchEventsRequest, stream grpc.ServerStreamingServer[pb.BrokerEvent]) error {
		cursors = append(cursors, req.AfterEventSeq)
		event := &pb.BrokerEvent{EventSeq: 7, Event: &pb.BrokerEvent_Snapshot{Snapshot: &pb.BrokerSnapshot{}}}
		if req.AfterEventSeq != 0 {
			event = &pb.BrokerEvent{EventSeq: 8, Event: &pb.BrokerEvent_StatusChanged{StatusChanged: &pb.BrokerStatus{Paused: true}}}
		}
		if err := stream.Send(event); err != nil {
			return err
		}
		return contract.Error(codes.Unavailable, pb.ErrorReason_STORAGE_UNAVAILABLE, "storage failed", nil)
	}}
	handler := newHandler(broker, DefaultPort)
	var lastID string
	for _, tt := range []struct {
		path, event, sequence string
	}{
		{"/events", "snapshot", "7"},
		{"/events", "status_changed", "8"},
	} {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:49783"+tt.path, nil)
		if lastID != "" {
			req.Header.Set("Last-Event-ID", lastID)
		}
		rec := &deadlineRecorder{httptest.NewRecorder()}
		handler.ServeHTTP(rec, req)
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/event-stream" || !rec.Flushed {
			t.Fatalf("stream status=%d headers=%v flushed=%v body=%s", rec.Code, rec.Header(), rec.Flushed, rec.Body)
		}
		body := rec.Body.String()
		idStart := strings.Index(body, "\nid: ")
		if idStart == -1 {
			t.Fatalf("missing SSE id: %s", body)
		}
		lastID = strings.SplitN(body[idStart+len("\nid: "):], "\n", 2)[0]
		if !strings.HasSuffix(lastID, ":"+tt.sequence) ||
			!strings.Contains(body, "event: "+tt.event+"\nid: "+lastID+"\ndata: {") ||
			!strings.Contains(body, "event: stream_error\ndata: {") ||
			!strings.Contains(body, "STORAGE_UNAVAILABLE") {
			t.Fatalf("unexpected SSE frames: %s", body)
		}
	}
	if len(cursors) != 2 || cursors[0] != 0 || cursors[1] != 7 {
		t.Fatalf("watch cursors = %v", cursors)
	}
}

func TestSSERejectsInvalidOrOldCursorBeforeStreaming(t *testing.T) {
	broker := &sseBroker{watch: func(_ *pb.WatchEventsRequest, _ grpc.ServerStreamingServer[pb.BrokerEvent]) error {
		return errors.New("watch must not start")
	}}
	handler := newHandler(broker, DefaultPort)
	for _, cursor := range []string{"abc", "123:0", "-1:3", "123:1:2", "00000000000000000000000000000000:1"} {
		req := httptest.NewRequest(http.MethodGet, "http://localhost:49783/events", nil)
		req.Header.Set("Last-Event-ID", cursor)
		rec := &deadlineRecorder{httptest.NewRecorder()}
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") {
			t.Fatalf("cursor %q: status=%d body=%s", cursor, rec.Code, rec.Body)
		}
	}
}

func TestSSEHeartbeatAndCursorPrecedence(t *testing.T) {
	const epoch = "0123456789abcdef0123456789abcdef"
	broker := &sseBroker{watch: func(req *pb.WatchEventsRequest, stream grpc.ServerStreamingServer[pb.BrokerEvent]) error {
		if req.AfterEventSeq != 9 {
			return fmt.Errorf("cursor %d, want 9", req.AfterEventSeq)
		}
		return stream.Send(&pb.BrokerEvent{EventSeq: 9, Event: &pb.BrokerEvent_Heartbeat{
			Heartbeat: &pb.Heartbeat{HeartbeatInterval: durationpb.New(5 * time.Second)},
		}})
	}}
	req := httptest.NewRequest(http.MethodGet, "http://localhost:49783/events?afterEventId="+url.QueryEscape(epoch+":2"), nil)
	req.Header.Set("Last-Event-ID", epoch+":9")
	rec := &deadlineRecorder{httptest.NewRecorder()}
	serveEvents(rec, req, broker, epoch, nil)
	body := rec.Body.String()
	if !strings.HasPrefix(body, "event: heartbeat\nid: "+epoch+":9\ndata: ") {
		t.Fatalf("heartbeat frame: %s", body)
	}
	data := strings.SplitN(strings.SplitN(body, "\ndata: ", 2)[1], "\n\n", 2)[0]
	var event pb.BrokerEvent
	if err := protojson.Unmarshal([]byte(data), &event); err != nil ||
		event.EventSeq != 9 || event.GetHeartbeat().GetHeartbeatInterval().AsDuration() != 5*time.Second {
		t.Fatalf("heartbeat payload = %s; %v", data, err)
	}
	if !strings.Contains(body, "event: stream_error\n") {
		t.Fatalf("unexpected stream end must be explicit: %s", body)
	}
}

func TestSSEEncodesMultilineTextInOneDataLine(t *testing.T) {
	const epoch = "0123456789abcdef0123456789abcdef"
	broker := &sseBroker{watch: func(_ *pb.WatchEventsRequest, stream grpc.ServerStreamingServer[pb.BrokerEvent]) error {
		return stream.Send(&pb.BrokerEvent{EventSeq: 3, Event: &pb.BrokerEvent_JobChanged{
			JobChanged: &pb.Job{Command: "echo first\nsecond"},
		}})
	}}
	req := httptest.NewRequest(http.MethodGet, "http://localhost:49783/events", nil)
	rec := &deadlineRecorder{httptest.NewRecorder()}
	serveEvents(rec, req, broker, epoch, nil)
	frames := strings.SplitN(rec.Body.String(), "\n\n", 2)
	lines := strings.Split(frames[0], "\n")
	if len(lines) != 3 || lines[0] != "event: job_changed" || lines[1] != "id: "+epoch+":3" ||
		!strings.HasPrefix(lines[2], "data: ") {
		t.Fatalf("malformed frame: %q", frames[0])
	}
	var event pb.BrokerEvent
	if err := protojson.Unmarshal([]byte(strings.TrimPrefix(lines[2], "data: ")), &event); err != nil ||
		event.GetJobChanged().GetCommand() != "echo first\nsecond" {
		t.Fatalf("invalid JSON event: %q (%v)", lines[2], err)
	}
}

func TestSSEZeroSequenceSnapshotRequestsFreshSnapshotOnReconnect(t *testing.T) {
	const epoch = "0123456789abcdef0123456789abcdef"
	broker := &sseBroker{watch: func(req *pb.WatchEventsRequest, stream grpc.ServerStreamingServer[pb.BrokerEvent]) error {
		if req.AfterEventSeq != 0 {
			return fmt.Errorf("cursor %d, want snapshot", req.AfterEventSeq)
		}
		if err := stream.Send(&pb.BrokerEvent{EventSeq: 0, Event: &pb.BrokerEvent_Snapshot{Snapshot: &pb.BrokerSnapshot{}}}); err != nil {
			return err
		}
		return contract.Error(codes.OutOfRange, pb.ErrorReason_EVENTS_EXPIRED, "refresh", nil)
	}}
	for _, cursor := range []string{"", epoch + ":0"} {
		req := httptest.NewRequest(http.MethodGet, "http://localhost:49783/events", nil)
		if cursor != "" {
			req.Header.Set("Last-Event-ID", cursor)
		}
		rec := &deadlineRecorder{httptest.NewRecorder()}
		serveEvents(rec, req, broker, epoch, nil)
		if rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), "event: snapshot\nid: "+epoch+":0\ndata: ") ||
			!strings.Contains(rec.Body.String(), "event: stream_error\n") {
			t.Fatalf("zero snapshot %q: status=%d body=%s", cursor, rec.Code, rec.Body)
		}
	}
}

func TestSSEStartupFailureAndInvalidRequests(t *testing.T) {
	const epoch = "0123456789abcdef0123456789abcdef"
	broker := &sseBroker{watch: func(_ *pb.WatchEventsRequest, _ grpc.ServerStreamingServer[pb.BrokerEvent]) error {
		return contract.Error(codes.OutOfRange, pb.ErrorReason_EVENTS_EXPIRED, "refresh", nil)
	}}
	for _, tt := range []struct {
		path, lastID string
		status       int
		reason       string
	}{
		{"/events?afterEventId=" + epoch + ":8", "", 400, "EVENTS_EXPIRED"},
		{"/events?afterEventId=" + epoch + ":8&afterEventId=" + epoch + ":9", "", 400, "INVALID_ARGUMENT_FIELD"},
		{"/events?afterEventId=", "", 400, "INVALID_ARGUMENT_FIELD"},
		{"/events?unknown=1", "", 400, "INVALID_ARGUMENT_FIELD"},
		{"/events", "ffffffffffffffffffffffffffffffff:1", 400, "EVENTS_EXPIRED"},
	} {
		req := httptest.NewRequest(http.MethodGet, "http://localhost:49783"+tt.path, nil)
		if tt.lastID != "" {
			req.Header.Set("Last-Event-ID", tt.lastID)
		}
		rec := &deadlineRecorder{httptest.NewRecorder()}
		serveEvents(rec, req, broker, epoch, nil)
		if rec.Code != tt.status || !strings.Contains(rec.Body.String(), tt.reason) ||
			rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s %q: status %d, body %s", tt.path, tt.lastID, rec.Code, rec.Body)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Errorf("error is not JSON: %v", err)
		}
	}
}

func TestSSEDisconnectCancelsWatch(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	broker := &sseBroker{watch: func(_ *pb.WatchEventsRequest, stream grpc.ServerStreamingServer[pb.BrokerEvent]) error {
		defer close(stopped)
		if err := stream.Send(&pb.BrokerEvent{EventSeq: 1, Event: &pb.BrokerEvent_Snapshot{Snapshot: &pb.BrokerSnapshot{}}}); err != nil {
			return err
		}
		close(started)
		<-stream.Context().Done()
		return stream.Context().Err()
	}}
	_, address := startProbe(t, broker)
	response, err := http.Get("http://" + address + "/events")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not stop after disconnect")
	}
}

func TestSSEHTTP2AndExistingAccessControls(t *testing.T) {
	broker := &sseBroker{watch: func(_ *pb.WatchEventsRequest, stream grpc.ServerStreamingServer[pb.BrokerEvent]) error {
		if err := stream.Send(&pb.BrokerEvent{EventSeq: 1, Event: &pb.BrokerEvent_Snapshot{Snapshot: &pb.BrokerSnapshot{}}}); err != nil {
			return err
		}
		<-stream.Context().Done()
		return stream.Context().Err()
	}}
	_, address := startProbe(t, broker)
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	transport := &http.Transport{Protocols: protocols}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	response, err := client.Get("http://" + address + "/events")
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, len("event: snapshot\n"))
	if _, err := io.ReadFull(response.Body, buffer); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.ProtoMajor != 2 || string(buffer) != "event: snapshot\n" ||
		response.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("HTTP/2 SSE = %s (%s), %q, headers %v", response.Status, response.Proto, buffer, response.Header)
	}

	handler := newHandler(broker, DefaultPort)
	req := httptest.NewRequest(http.MethodGet, "http://example.test:49783/events", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-loopback Host status = %d", rec.Code)
	}
	preflight := httptest.NewRequest(http.MethodOptions, "http://localhost:49783/events", nil)
	preflight.Header.Set("Origin", "https://example.test")
	preflight.Header.Set("Access-Control-Request-Method", "GET")
	preflight.Header.Set("Access-Control-Request-Headers", "authorization")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, preflight)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unsupported preflight header status = %d", rec.Code)
	}
}

type failingWriter struct {
	header   http.Header
	deadline time.Time
	writes   int
}

func (w *failingWriter) Header() http.Header                { return w.header }
func (*failingWriter) WriteHeader(int)                      {}
func (w *failingWriter) Write([]byte) (int, error)          { w.writes++; return 0, io.ErrClosedPipe }
func (*failingWriter) Flush()                               {}
func (w *failingWriter) SetWriteDeadline(t time.Time) error { w.deadline = t; return nil }

func TestSSESlowWriterStopsStream(t *testing.T) {
	writer := &failingWriter{header: make(http.Header)}
	watchStopped := false
	broker := &sseBroker{watch: func(_ *pb.WatchEventsRequest, stream grpc.ServerStreamingServer[pb.BrokerEvent]) error {
		defer func() { watchStopped = true }()
		return stream.Send(&pb.BrokerEvent{EventSeq: 1, Event: &pb.BrokerEvent_Snapshot{Snapshot: &pb.BrokerSnapshot{}}})
	}}
	before := time.Now()
	req := httptest.NewRequest(http.MethodGet, "http://localhost:49783/events", nil)
	serveEvents(writer, req, broker, "0123456789abcdef0123456789abcdef", nil)
	if !watchStopped || writer.writes != 1 || writer.deadline.Before(before) || writer.deadline.After(time.Now().Add(sseWriteTimeout)) {
		t.Fatalf("watch stopped=%v, writes=%d, write deadline=%v", watchStopped, writer.writes, writer.deadline)
	}
}

type pipeWriter struct {
	net.Conn
	header http.Header
}

func (w *pipeWriter) Header() http.Header { return w.header }
func (*pipeWriter) WriteHeader(int)       {}
func (*pipeWriter) Flush()                {}

func TestSSESlowReaderTimesOutWithoutBlockingAnotherClient(t *testing.T) {
	writerConn, readerConn := net.Pipe()
	defer writerConn.Close()
	defer readerConn.Close()
	writer := &pipeWriter{Conn: writerConn, header: make(http.Header)}
	started := make(chan struct{}, 2)
	broker := &sseBroker{watch: func(_ *pb.WatchEventsRequest, stream grpc.ServerStreamingServer[pb.BrokerEvent]) error {
		started <- struct{}{}
		return stream.Send(&pb.BrokerEvent{EventSeq: 1, Event: &pb.BrokerEvent_Snapshot{Snapshot: &pb.BrokerSnapshot{}}})
	}}
	done := make(chan struct{})
	before := time.Now()
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodGet, "http://localhost:49783/events", nil)
		serveEvents(writer, req, broker, "0123456789abcdef0123456789abcdef", nil)
	}()
	<-started
	fastDone := make(chan struct{})
	go func() {
		defer close(fastDone)
		req := httptest.NewRequest(http.MethodGet, "http://localhost:49783/events", nil)
		serveEvents(&deadlineRecorder{httptest.NewRecorder()}, req, broker, "0123456789abcdef0123456789abcdef", nil)
	}()
	select {
	case <-fastDone:
	case <-time.After(2 * time.Second):
		t.Fatal("a stalled subscriber blocked another client")
	}
	select {
	case <-done:
		if elapsed := time.Since(before); elapsed < sseWriteTimeout-time.Second {
			t.Fatalf("slow reader timed out too early after %s", elapsed)
		}
	case <-time.After(sseWriteTimeout + 5*time.Second):
		t.Fatal("slow reader did not time out and unsubscribe")
	}
}
