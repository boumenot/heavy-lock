package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	heavylockv2 "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type recordingBroker struct {
	heavylockv2.UnimplementedRunnerServiceServer
	heavylockv2.UnimplementedControlServiceServer
	called  string
	request proto.Message
}

func (b *recordingBroker) record(name string, request proto.Message) {
	b.called = name
	b.request = proto.Clone(request)
}

func (b *recordingBroker) GetHealth(_ context.Context, r *heavylockv2.GetHealthRequest) (*heavylockv2.Health, error) {
	b.record("GetHealth", r)
	return &heavylockv2.Health{Live: true, StartedAtUnixNs: 1764121200000000000}, nil
}
func (b *recordingBroker) GetStatus(_ context.Context, r *heavylockv2.GetStatusRequest) (*heavylockv2.BrokerStatus, error) {
	b.record("GetStatus", r)
	return &heavylockv2.BrokerStatus{}, nil
}
func (b *recordingBroker) GetQueue(_ context.Context, r *heavylockv2.GetQueueRequest) (*heavylockv2.Queue, error) {
	b.record("GetQueue", r)
	return &heavylockv2.Queue{}, nil
}
func (b *recordingBroker) ListJobs(_ context.Context, r *heavylockv2.ListJobsRequest) (*heavylockv2.ListJobsResponse, error) {
	b.record("ListJobs", r)
	return &heavylockv2.ListJobsResponse{}, nil
}
func (b *recordingBroker) GetJob(_ context.Context, r *heavylockv2.GetJobRequest) (*heavylockv2.Job, error) {
	b.record("GetJob", r)
	return &heavylockv2.Job{JobId: r.JobId}, nil
}
func (b *recordingBroker) GetSubmission(_ context.Context, r *heavylockv2.GetSubmissionRequest) (*heavylockv2.Job, error) {
	b.record("GetSubmission", r)
	return &heavylockv2.Job{IdempotencyKey: r.IdempotencyKey}, nil
}
func (b *recordingBroker) CancelJob(_ context.Context, r *heavylockv2.CancelJobRequest) (*heavylockv2.Job, error) {
	b.record("CancelJob", r)
	return &heavylockv2.Job{JobId: r.JobId}, nil
}
func (b *recordingBroker) MoveJob(_ context.Context, r *heavylockv2.MoveJobRequest) (*heavylockv2.Queue, error) {
	b.record("MoveJob", r)
	return &heavylockv2.Queue{}, nil
}
func (b *recordingBroker) ForceStartJob(_ context.Context, r *heavylockv2.ForceStartJobRequest) (*heavylockv2.Job, error) {
	b.record("ForceStartJob", r)
	return &heavylockv2.Job{JobId: r.JobId}, nil
}
func (b *recordingBroker) PauseQueue(_ context.Context, r *heavylockv2.PauseQueueRequest) (*heavylockv2.BrokerStatus, error) {
	b.record("PauseQueue", r)
	return &heavylockv2.BrokerStatus{}, nil
}
func (b *recordingBroker) ResumeQueue(_ context.Context, r *heavylockv2.ResumeQueueRequest) (*heavylockv2.BrokerStatus, error) {
	b.record("ResumeQueue", r)
	return &heavylockv2.BrokerStatus{}, nil
}
func (b *recordingBroker) DeleteJob(_ context.Context, r *heavylockv2.DeleteJobRequest) (*heavylockv2.DeleteJobResponse, error) {
	b.record("DeleteJob", r)
	return &heavylockv2.DeleteJobResponse{}, nil
}
func (b *recordingBroker) DeleteHistory(_ context.Context, r *heavylockv2.DeleteHistoryRequest) (*heavylockv2.DeleteHistoryResponse, error) {
	b.record("DeleteHistory", r)
	return &heavylockv2.DeleteHistoryResponse{}, nil
}

func TestRESTUnaryMappings(t *testing.T) {
	tests := []struct {
		method  string
		path    string
		body    string
		call    string
		request proto.Message
	}{
		{http.MethodGet, "/v1/health", "", "GetHealth", &heavylockv2.GetHealthRequest{}},
		{http.MethodGet, "/v1/status", "", "GetStatus", &heavylockv2.GetStatusRequest{}},
		{http.MethodGet, "/v1/queue", "", "GetQueue", &heavylockv2.GetQueueRequest{}},
		{http.MethodGet, "/v1/jobs?states=JOB_STATE_QUEUED&states=JOB_STATE_RUNNING&repository=repo&clientName=cli&queuedAfterUnixNs=42&queuedBeforeUnixNs=99&pageSize=50&pageToken=next",
			"", "ListJobs", &heavylockv2.ListJobsRequest{
				States:     []heavylockv2.JobState{heavylockv2.JobState_JOB_STATE_QUEUED, heavylockv2.JobState_JOB_STATE_RUNNING},
				Repository: proto.String("repo"), ClientName: proto.String("cli"), QueuedAfterUnixNs: proto.Int64(42),
				QueuedBeforeUnixNs: proto.Int64(99), PageSize: 50, PageToken: "next",
			}},
		{http.MethodGet, "/v1/jobs/job-1", "", "GetJob", &heavylockv2.GetJobRequest{JobId: "job-1"}},
		{http.MethodGet, "/v1/submissions/key-1", "", "GetSubmission", &heavylockv2.GetSubmissionRequest{IdempotencyKey: "key-1"}},
		{http.MethodPost, "/v1/jobs/job-1/cancel", `{"reason":"stop"}`, "CancelJob", &heavylockv2.CancelJobRequest{JobId: "job-1", Reason: proto.String("stop")}},
		{http.MethodPost, "/v1/jobs/job-1/move", `{"toFront":true}`, "MoveJob", &heavylockv2.MoveJobRequest{JobId: "job-1", Target: &heavylockv2.MoveJobRequest_ToFront{ToFront: true}}},
		{http.MethodPost, "/v1/jobs/job-1/force-start", "{}", "ForceStartJob", &heavylockv2.ForceStartJobRequest{JobId: "job-1"}},
		{http.MethodPost, "/v1/queue/pause", "", "PauseQueue", &heavylockv2.PauseQueueRequest{}},
		{http.MethodPost, "/v1/queue/resume", "{}", "ResumeQueue", &heavylockv2.ResumeQueueRequest{}},
		{http.MethodDelete, "/v1/jobs/job-1", "", "DeleteJob", &heavylockv2.DeleteJobRequest{JobId: "job-1"}},
		{http.MethodDelete, "/v1/history?finishedBeforeUnixNs=42", "", "DeleteHistory", &heavylockv2.DeleteHistoryRequest{FinishedBeforeUnixNs: 42}},
	}
	for _, tt := range tests {
		t.Run(tt.call, func(t *testing.T) {
			broker := &recordingBroker{}
			request := httptest.NewRequest(tt.method, "http://127.0.0.1:49783"+tt.path, strings.NewReader(tt.body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			newHandler(broker, DefaultPort).ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
			}
			if broker.called != tt.call || !proto.Equal(broker.request, tt.request) {
				t.Fatalf("called %s with %v, want %s with %v", broker.called, broker.request, tt.call, tt.request)
			}
			if tt.call == "GetHealth" && !strings.Contains(response.Body.String(), `"startedAtUnixNs":"1764121200000000000"`) {
				t.Fatalf("int64 timestamp must be a JSON string: %s", response.Body.String())
			}
		})
	}
}

func errorReason(t *testing.T, response *httptest.ResponseRecorder) (codes.Code, string, map[string]string) {
	t.Helper()
	var result statuspb.Status
	if err := protojson.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("invalid google.rpc.Status %q: %v", response.Body.String(), err)
	}
	for _, detail := range result.Details {
		var info errdetails.ErrorInfo
		if err := detail.UnmarshalTo(&info); err == nil && info.Domain == "heavylock.v2" {
			return codes.Code(result.Code), info.Reason, info.Metadata
		}
	}
	t.Fatalf("missing heavylock ErrorInfo in %s", response.Body.String())
	return codes.Unknown, "", nil
}

func TestRESTRejectsAmbiguousAndUnsupportedRequests(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		path        string
		body        string
		contentType string
		httpCode    int
		grpcCode    codes.Code
		reason      heavylockv2.ErrorReason
		field       string
	}{
		{"unknown route", http.MethodGet, "/v1/nope", "", "", 404, codes.NotFound, heavylockv2.ErrorReason_HTTP_ROUTE_NOT_FOUND, ""},
		{"runner is gRPC-only", http.MethodPost, "/v1/runner/submit", "", "", 404, codes.NotFound, heavylockv2.ErrorReason_HTTP_ROUTE_NOT_FOUND, ""},
		{"method", http.MethodPost, "/v1/status", "", "", 405, codes.Unimplemented, heavylockv2.ErrorReason_HTTP_METHOD_NOT_ALLOWED, ""},
		{"duplicate query", http.MethodGet, "/v1/jobs?pageSize=1&pageSize=2", "", "", 400, codes.InvalidArgument, heavylockv2.ErrorReason_INVALID_ARGUMENT_FIELD, "pageSize"},
		{"unknown query", http.MethodGet, "/v1/jobs?unknown=1", "", "", 400, codes.InvalidArgument, heavylockv2.ErrorReason_INVALID_ARGUMENT_FIELD, "unknown"},
		{"overflow", http.MethodDelete, "/v1/history?finishedBeforeUnixNs=9223372036854775808", "", "", 400, codes.InvalidArgument, heavylockv2.ErrorReason_INVALID_ARGUMENT_FIELD, "finishedBeforeUnixNs"},
		{"path conflict", http.MethodPost, "/v1/jobs/job-1/cancel", `{"jobId":"job-2"}`, "application/json", 400, codes.InvalidArgument, heavylockv2.ErrorReason_INVALID_ARGUMENT_FIELD, "jobId"},
		{"oneof conflict", http.MethodPost, "/v1/jobs/job-1/move", `{"toFront":true,"toBack":true}`, "application/json", 400, codes.InvalidArgument, heavylockv2.ErrorReason_INVALID_ARGUMENT_FIELD, "body"},
		{"content type", http.MethodPost, "/v1/jobs/job-1/cancel", `{"reason":"x"}`, "text/plain", 415, codes.InvalidArgument, heavylockv2.ErrorReason_HTTP_UNSUPPORTED_MEDIA_TYPE, ""},
		{"body limit", http.MethodPost, "/v1/queue/pause", strings.Repeat("x", maxRESTBodyBytes+1), "application/json", 413, codes.ResourceExhausted, heavylockv2.ErrorReason_HTTP_REQUEST_TOO_LARGE, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			broker := &recordingBroker{}
			request := httptest.NewRequest(tt.method, "http://127.0.0.1:49783"+tt.path, strings.NewReader(tt.body))
			request.Header.Set("Origin", "https://any.example")
			if tt.contentType != "" {
				request.Header.Set("Content-Type", tt.contentType)
			}
			response := httptest.NewRecorder()
			newHandler(broker, DefaultPort).ServeHTTP(response, request)
			if response.Code != tt.httpCode || response.Header().Get("Access-Control-Allow-Origin") != "*" {
				t.Fatalf("status = %d, headers = %v, body = %q", response.Code, response.Header(), response.Body.String())
			}
			code, reason, metadata := errorReason(t, response)
			if code != tt.grpcCode || reason != tt.reason.String() || metadata["field"] != tt.field {
				t.Fatalf("error = %v/%s/%v", code, reason, metadata)
			}
			if broker.called != "" {
				t.Fatalf("invalid request reached %s", broker.called)
			}
		})
	}
}

func TestRESTInvalidFieldSelectionIsDeterministic(t *testing.T) {
	handler := newHandler(&recordingBroker{}, DefaultPort)
	for i := 0; i < 32; i++ {
		request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:49783/v1/jobs?z=1&a=1", nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		_, _, metadata := errorReason(t, response)
		if metadata["field"] != "a" {
			t.Fatalf("first invalid field = %q, want a", metadata["field"])
		}
	}
}
