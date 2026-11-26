package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/boumenot/heavy-lock/apps/tuna/contract"
	heavylockv2 "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type fakeBroker struct {
	heavylockv2.UnimplementedRunnerServiceServer
	heavylockv2.UnimplementedControlServiceServer
	mu     sync.Mutex
	paused bool
}

func (b *fakeBroker) GetHealth(context.Context, *heavylockv2.GetHealthRequest) (*heavylockv2.Health, error) {
	return &heavylockv2.Health{
		Live: true, Ready: false, ServiceVersion: "probe",
		ServiceIdentity: contract.ServiceIdentity, ApiMajor: contract.APIMajor, ApiMinor: contract.APIMinor,
	}, nil
}

func (b *fakeBroker) GetStatus(context.Context, *heavylockv2.GetStatusRequest) (*heavylockv2.BrokerStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return &heavylockv2.BrokerStatus{Paused: b.paused}, nil
}

func (b *fakeBroker) PauseQueue(context.Context, *heavylockv2.PauseQueueRequest) (*heavylockv2.BrokerStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.paused = true
	return &heavylockv2.BrokerStatus{Paused: true}, nil
}

func (b *fakeBroker) ResumeQueue(context.Context, *heavylockv2.ResumeQueueRequest) (*heavylockv2.BrokerStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.paused = false
	return &heavylockv2.BrokerStatus{}, nil
}

func (b *fakeBroker) CancelJob(context.Context, *heavylockv2.CancelJobRequest) (*heavylockv2.Job, error) {
	return nil, contract.Error(codes.FailedPrecondition, heavylockv2.ErrorReason_JOB_NOT_WAITING, "job is not waiting", nil)
}

func (b *fakeBroker) WatchEvents(_ *heavylockv2.WatchEventsRequest, stream grpc.ServerStreamingServer[heavylockv2.BrokerEvent]) error {
	return stream.Send(&heavylockv2.BrokerEvent{EventSeq: 42})
}

func (b *fakeBroker) TrySubmitAndWatch(_ *heavylockv2.SubmitJobRequest, stream grpc.ServerStreamingServer[heavylockv2.JobEvent]) error {
	return stream.Send(&heavylockv2.JobEvent{EventSeq: 7})
}

func TestRESTDispatchesToBroker(t *testing.T) {
	broker := &fakeBroker{}
	handler := newHandler(broker, DefaultPort)
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:49783/v1/queue/pause", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("pause status = %d, body = %q", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("allow origin = %q, want *", got)
	}
	var result heavylockv2.BrokerStatus
	if err := protojson.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Paused {
		t.Fatal("pause did not use the shared broker")
	}
}

func TestRESTPreflightAndHostValidation(t *testing.T) {
	handler := newHandler(&fakeBroker{}, DefaultPort)
	preflight := httptest.NewRequest(http.MethodOptions, "http://localhost:49783/v1/queue/pause", nil)
	preflight.Header.Set("Origin", "https://example.test")
	preflight.Header.Set("Access-Control-Request-Method", "POST")
	preflight.Header.Set("Access-Control-Request-Headers", "content-type")
	preflight.Header.Set("Access-Control-Request-Private-Network", "true")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, preflight)
	if response.Code != http.StatusNoContent || response.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("preflight = %d, headers = %v, body = %q", response.Code, response.Header(), response.Body.String())
	}
	if response.Header().Get("Access-Control-Allow-Private-Network") != "true" ||
		!strings.Contains(strings.Join(response.Header().Values("Vary"), ","), "Access-Control-Request-Method") {
		t.Fatalf("preflight did not advertise private-network access and cache variation: %v", response.Header())
	}

	request := httptest.NewRequest(http.MethodGet, "http://example.test:49783/v1/status", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("non-loopback Host status = %d, want 400", response.Code)
	}
}

func TestRESTAuthoritiesAndPreflightErrors(t *testing.T) {
	handler := newHandler(&fakeBroker{}, DefaultPort)
	for _, tt := range []struct {
		host string
		code int
	}{
		{"127.0.0.1:49783", 200},
		{"LOCALHOST:49783", 200},
		{"localhost", 200},
		{"127.0.0.1:49784", 400},
		{"localhost.evil.example:49783", 400},
		{"localhost:", 400},
		{"[::1]:49783", 400},
	} {
		t.Run(tt.host, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:49783/v1/status", nil)
			request.Host = tt.host
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tt.code {
				t.Fatalf("status = %d, want %d", response.Code, tt.code)
			}
			if tt.code != 200 {
				code, reason, metadata := errorReason(t, response)
				if code != codes.InvalidArgument || reason != heavylockv2.ErrorReason_INVALID_ARGUMENT_FIELD.String() || metadata["field"] != "host" {
					t.Fatalf("host error = %v/%s/%v", code, reason, metadata)
				}
			}
		})
	}
	for _, tt := range []struct {
		name   string
		method string
		header string
		code   int
	}{
		{"unsupported method", "DELETE", "content-type", 405},
		{"unsupported header", "POST", "authorization", 400},
	} {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodOptions, "http://localhost:49783/v1/queue/pause", nil)
			request.Header.Set("Origin", "https://any.example")
			request.Header.Set("Access-Control-Request-Method", tt.method)
			request.Header.Set("Access-Control-Request-Headers", tt.header)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tt.code || response.Header().Get("Access-Control-Allow-Origin") != "*" {
				t.Fatalf("preflight = %d, headers = %v", response.Code, response.Header())
			}
			errorReason(t, response)
		})
	}
}

func startProbe(t *testing.T, broker Broker) (*Server, string) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := newServer(listener, broker)
	finished := make(chan error, 1)
	go func() { finished <- server.Serve() }()
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("close probe: %v", err)
		}
		select {
		case err := <-finished:
			if err != nil {
				t.Errorf("serve probe: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("probe did not stop")
		}
	})
	return server, net.JoinHostPort("127.0.0.1", strconv.Itoa(server.Port()))
}

func TestSharedPortGRPCAndREST(t *testing.T) {
	_, address := startProbe(t, &fakeBroker{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	control := heavylockv2.NewControlServiceClient(conn)
	health, err := control.GetHealth(ctx, &heavylockv2.GetHealthRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if err := contract.CheckService(health, 0); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	preflight, err := http.NewRequest(http.MethodOptions, "http://"+address+"/v1/queue/pause", nil)
	if err != nil {
		t.Fatal(err)
	}
	preflight.Header.Set("Origin", "https://example.test")
	preflight.Header.Set("Access-Control-Request-Method", "POST")
	preflight.Header.Set("Access-Control-Request-Headers", "content-type")
	preflight.Header.Set("Access-Control-Request-Private-Network", "true")
	response, err := client.Do(preflight)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent || response.Header.Get("Access-Control-Allow-Origin") != "*" ||
		response.Header.Get("Access-Control-Allow-Private-Network") != "true" {
		t.Fatalf("network preflight = %s, headers = %v", response.Status, response.Header)
	}
	response, err = client.Post("http://"+address+"/v1/queue/pause", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 || response.ProtoMajor != 1 {
		t.Fatalf("HTTP/1 pause = %s (%s)", response.Status, response.Proto)
	}
	state, err := control.GetStatus(ctx, &heavylockv2.GetStatusRequest{})
	if err != nil || !state.GetPaused() {
		t.Fatalf("gRPC status after REST pause = %v, %v", state, err)
	}
	if _, err := control.ResumeQueue(ctx, &heavylockv2.ResumeQueueRequest{}); err != nil {
		t.Fatal(err)
	}

	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	h2Transport := &http.Transport{Protocols: protocols}
	defer h2Transport.CloseIdleConnections()
	h2Client := &http.Client{Transport: h2Transport, Timeout: 5 * time.Second}
	response, err = h2Client.Get("http://" + address + "/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var restState heavylockv2.BrokerStatus
	if err := protojson.Unmarshal(body, &restState); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || response.ProtoMajor != 2 || restState.Paused {
		t.Fatalf("HTTP/2 status = %s (%s): %s", response.Status, response.Proto, body)
	}

	events, err := control.WatchEvents(ctx, &heavylockv2.WatchEventsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	event, err := events.Recv()
	if err != nil || event.GetEventSeq() != 42 {
		t.Fatalf("gRPC event = %v, %v", event, err)
	}
	runner := heavylockv2.NewRunnerServiceClient(conn)
	jobEvents, err := runner.TrySubmitAndWatch(ctx, &heavylockv2.SubmitJobRequest{})
	if err != nil {
		t.Fatal(err)
	}
	jobEvent, err := jobEvents.Recv()
	if err != nil || jobEvent.GetEventSeq() != 7 {
		t.Fatalf("gRPC runner event = %v, %v", jobEvent, err)
	}

	_, grpcErr := control.CancelJob(ctx, &heavylockv2.CancelJobRequest{JobId: "job-1"})
	if status.Code(grpcErr) != codes.FailedPrecondition {
		t.Fatalf("gRPC error = %v", grpcErr)
	}
	response, err = client.Post("http://"+address+"/v1/jobs/job-1/cancel", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var restError statuspb.Status
	if err := protojson.Unmarshal(body, &restError); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 400 || !proto.Equal(&restError, status.Convert(grpcErr).Proto()) {
		t.Fatalf("REST error = %s %s, gRPC = %v", response.Status, body, grpcErr)
	}
}

func TestListenerConflictDoesNotChooseAnotherPort(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	if server, err := Listen(port, &fakeBroker{}); !errors.Is(err, ErrPortInUse) {
		if server != nil {
			server.Close()
		}
		t.Fatalf("listen on occupied port %d = %v", port, err)
	}
	if _, err := Listen(0, &fakeBroker{}); err == nil {
		t.Fatal("port zero must not silently choose another port")
	}
}

func TestGRPCRejectsUnrelatedAuthority(t *testing.T) {
	_, address := startProbe(t, &fakeBroker{})
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithAuthority(fmt.Sprintf("unrelated.example:%d", DefaultPort)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = heavylockv2.NewControlServiceClient(conn).GetStatus(ctx, &heavylockv2.GetStatusRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("gRPC authority error = %v", err)
	}
	details := status.Convert(err).Details()
	if len(details) != 1 {
		t.Fatalf("gRPC authority details = %v", details)
	}
	info, ok := details[0].(*errdetails.ErrorInfo)
	if !ok || info.Domain != contract.ServiceIdentity ||
		info.Reason != heavylockv2.ErrorReason_INVALID_ARGUMENT_FIELD.String() || info.Metadata["field"] != "host" {
		t.Fatalf("gRPC authority detail = %v", details[0])
	}
}

func TestSwiftGRPCCompatibility(t *testing.T) {
	if os.Getenv("TUNA_TEST_SWIFT") != "1" {
		t.Skip("run explicitly through the shared gate with TUNA_TEST_SWIFT=1")
	}
	broker := &fakeBroker{}
	server, _ := startProbe(t, broker)
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "swift", "test",
		"--package-path", filepath.Join(root, "packages", "heavylock-v2-swift"),
		"--only-use-versions-from-resolved-file", "--filter", "swiftGRPCSharedPort")
	cmd.Env = append(os.Environ(), "TUNA_TEST_SWIFT_PORT="+strconv.Itoa(server.Port()))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Swift gRPC probe failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "Test swiftGRPCSharedPort() passed") {
		t.Fatalf("Swift gRPC probe did not run:\n%s", output)
	}
	state, err := broker.GetStatus(ctx, &heavylockv2.GetStatusRequest{})
	if err != nil || !state.Paused {
		t.Fatalf("Swift did not affect the shared broker: %v, %v", state, err)
	}
}
