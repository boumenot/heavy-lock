package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/boumenot/tuna/apps/tuna/contract"
	tunapb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type recordingBroker struct {
	tunapb.UnimplementedRunnerServiceServer
	tunapb.UnimplementedControlServiceServer
	called  string
	request proto.Message
	job     *tunapb.Job
	status  *tunapb.BrokerStatus
}

func (b *recordingBroker) record(name string, request proto.Message) {
	b.called = name
	b.request = proto.Clone(request)
}

func (b *recordingBroker) GetHealth(_ context.Context, r *tunapb.GetHealthRequest) (*tunapb.Health, error) {
	b.record("GetHealth", r)
	return &tunapb.Health{Live: true, StartedAt: contract.Timestamp(1764121200000000000)}, nil
}
func (b *recordingBroker) GetStatus(_ context.Context, r *tunapb.GetStatusRequest) (*tunapb.BrokerStatus, error) {
	b.record("GetStatus", r)
	if b.status != nil {
		return proto.Clone(b.status).(*tunapb.BrokerStatus), nil
	}
	return &tunapb.BrokerStatus{}, nil
}
func (b *recordingBroker) GetQueue(_ context.Context, r *tunapb.GetQueueRequest) (*tunapb.Queue, error) {
	b.record("GetQueue", r)
	return &tunapb.Queue{}, nil
}
func (b *recordingBroker) ListJobs(_ context.Context, r *tunapb.ListJobsRequest) (*tunapb.ListJobsResponse, error) {
	b.record("ListJobs", r)
	return &tunapb.ListJobsResponse{}, nil
}
func (b *recordingBroker) GetJob(_ context.Context, r *tunapb.GetJobRequest) (*tunapb.Job, error) {
	b.record("GetJob", r)
	if b.job != nil {
		return proto.Clone(b.job).(*tunapb.Job), nil
	}
	return &tunapb.Job{JobId: r.JobId}, nil
}
func (b *recordingBroker) GetSubmission(_ context.Context, r *tunapb.GetSubmissionRequest) (*tunapb.Job, error) {
	b.record("GetSubmission", r)
	return &tunapb.Job{IdempotencyKey: r.IdempotencyKey}, nil
}
func (b *recordingBroker) CancelJob(_ context.Context, r *tunapb.CancelJobRequest) (*tunapb.Job, error) {
	b.record("CancelJob", r)
	return &tunapb.Job{JobId: r.JobId}, nil
}
func (b *recordingBroker) MoveJob(_ context.Context, r *tunapb.MoveJobRequest) (*tunapb.Queue, error) {
	b.record("MoveJob", r)
	return &tunapb.Queue{}, nil
}
func (b *recordingBroker) ForceStartJob(_ context.Context, r *tunapb.ForceStartJobRequest) (*tunapb.Job, error) {
	b.record("ForceStartJob", r)
	return &tunapb.Job{JobId: r.JobId}, nil
}
func (b *recordingBroker) PauseQueue(_ context.Context, r *tunapb.PauseQueueRequest) (*tunapb.BrokerStatus, error) {
	b.record("PauseQueue", r)
	return &tunapb.BrokerStatus{}, nil
}
func (b *recordingBroker) ResumeQueue(_ context.Context, r *tunapb.ResumeQueueRequest) (*tunapb.BrokerStatus, error) {
	b.record("ResumeQueue", r)
	return &tunapb.BrokerStatus{}, nil
}
func (b *recordingBroker) DeleteJob(_ context.Context, r *tunapb.DeleteJobRequest) (*tunapb.DeleteJobResponse, error) {
	b.record("DeleteJob", r)
	return &tunapb.DeleteJobResponse{}, nil
}
func (b *recordingBroker) DeleteHistory(_ context.Context, r *tunapb.DeleteHistoryRequest) (*tunapb.DeleteHistoryResponse, error) {
	b.record("DeleteHistory", r)
	return &tunapb.DeleteHistoryResponse{}, nil
}

func TestRESTUnaryMappings(t *testing.T) {
	tests := []struct {
		method  string
		path    string
		body    string
		call    string
		request proto.Message
	}{
		{http.MethodGet, "/health", "", "GetHealth", &tunapb.GetHealthRequest{}},
		{http.MethodGet, "/status", "", "GetStatus", &tunapb.GetStatusRequest{}},
		{http.MethodGet, "/queue", "", "GetQueue", &tunapb.GetQueueRequest{}},
		{http.MethodGet, "/jobs?states=JOB_STATE_QUEUED&states=JOB_STATE_RUNNING&repository=repo&clientName=cli&queuedAfter=1970-01-01T00:00:00.000000042Z&queuedBefore=1970-01-01T00:00:00.000000099Z&pageSize=50&pageToken=next",
			"", "ListJobs", &tunapb.ListJobsRequest{
				States:     []tunapb.JobState{tunapb.JobState_JOB_STATE_QUEUED, tunapb.JobState_JOB_STATE_RUNNING},
				Repository: proto.String("repo"), ClientName: proto.String("cli"), QueuedAfter: contract.Timestamp(42),
				QueuedBefore: contract.Timestamp(99), PageSize: 50, PageToken: "next",
			}},
		{http.MethodGet, "/jobs/job-1", "", "GetJob", &tunapb.GetJobRequest{JobId: "job-1"}},
		{http.MethodGet, "/submissions/key-1", "", "GetSubmission", &tunapb.GetSubmissionRequest{IdempotencyKey: "key-1"}},
		{http.MethodPost, "/jobs/job-1/cancel", `{"reason":"stop"}`, "CancelJob", &tunapb.CancelJobRequest{JobId: "job-1", Reason: proto.String("stop")}},
		{http.MethodPost, "/jobs/job-1/move", `{"toFront":true}`, "MoveJob", &tunapb.MoveJobRequest{JobId: "job-1", Target: &tunapb.MoveJobRequest_ToFront{ToFront: true}}},
		{http.MethodPost, "/jobs/job-1/force-start", "{}", "ForceStartJob", &tunapb.ForceStartJobRequest{JobId: "job-1"}},
		{http.MethodPost, "/queue/pause", "", "PauseQueue", &tunapb.PauseQueueRequest{}},
		{http.MethodPost, "/queue/resume", "{}", "ResumeQueue", &tunapb.ResumeQueueRequest{}},
		{http.MethodDelete, "/jobs/job-1", "", "DeleteJob", &tunapb.DeleteJobRequest{JobId: "job-1"}},
		{http.MethodDelete, "/history?finishedBefore=1970-01-01T00:00:00.000000042Z", "", "DeleteHistory", &tunapb.DeleteHistoryRequest{FinishedBefore: contract.Timestamp(42)}},
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
			if tt.call == "GetHealth" {
				var object map[string]any
				if err := json.Unmarshal(response.Body.Bytes(), &object); err != nil || object["startedAt"] != "2025-11-26T01:40:00Z" {
					t.Fatalf("timestamp must be RFC 3339: %s (%v)", response.Body.String(), err)
				}
			}
		})
	}
}

func TestRESTJSONGoldenShapes(t *testing.T) {
	tests := []struct {
		name, path, golden string
		broker             *recordingBroker
	}{
		{
			name:   "health defaults",
			path:   "/health",
			broker: &recordingBroker{},
			golden: `{"live":true,"ready":false,"serviceVersion":"","startedAt":"2025-11-26T01:40:00Z",
				"schemaVersion":"0","serviceIdentity":""}`,
		},
		{
			name:   "status defaults and absent optionals",
			path:   "/status",
			broker: &recordingBroker{},
			golden: `{"capacity":0,"occupied":0,"paused":false,"recovering":false,"queuedJobs":0,"runningJobs":0,
				"headBlockedReason":"BLOCKED_REASON_UNSPECIFIED","storageUnavailable":false,
				"unreconciledReservations":0,"backfillLimit":0,"queueMode":"QUEUE_MODE_UNSPECIFIED",
				"queueState":"QUEUE_STATE_UNSPECIFIED"}`,
		},
		{
			name: "timestamp and duration presence",
			path: "/jobs/job",
			broker: &recordingBroker{job: &tunapb.Job{
				JobId: "job", QueuedAt: contract.Timestamp(1764121200123456789),
				Resources: &tunapb.ResourceSummary{Elapsed: contract.MillisecondDuration(0), CpuTime: contract.MillisecondDuration(1234)},
			}},
			golden: `{"jobId":"job","idempotencyKey":"","state":"JOB_STATE_UNSPECIFIED","command":"","weight":0,
				"forced":false,"serviceVersion":"","queuedAt":"2025-11-26T01:40:00.123456789Z",
				"runnerConnected":false,"resources":{"elapsed":"0s","cpuTime":"1.234s","sampleCount":0,
				"completeness":"MEASUREMENT_COMPLETENESS_UNSPECIFIED"},"admittedByBackfill":false,
				"backfillBypassCount":0,"attempt":0,"blockedReason":"BLOCKED_REASON_UNSPECIFIED"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			newHandler(tt.broker, DefaultPort).ServeHTTP(
				response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:49783"+tt.path, nil))
			var got, want any
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tt.golden), &want); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusOK || !reflect.DeepEqual(got, want) {
				t.Fatalf("status %d, JSON %s; want %s", response.Code, response.Body.String(), tt.golden)
			}
		})
	}
}

func TestRESTStatusLoadAveragesJSONOrder(t *testing.T) {
	tests := []struct {
		name  string
		loads [3]*float64
	}{
		{"all present", [3]*float64{proto.Float64(1.25), proto.Float64(2.5), proto.Float64(3.75)}},
		{"explicit zeros", [3]*float64{proto.Float64(0), proto.Float64(0), proto.Float64(0)}},
		{"one and fifteen only", [3]*float64{proto.Float64(0), nil, proto.Float64(3.75)}},
		{"five and fifteen only", [3]*float64{nil, proto.Float64(2.5), proto.Float64(3.75)}},
		{"all absent", [3]*float64{}},
	}
	loadNames := []string{"loadAverage1m", "loadAverage5m", "loadAverage15m"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := &tunapb.BrokerStatus{
				Capacity: 6, Occupied: 2, Paused: true, PausedAt: contract.Timestamp(1764121200000000000),
				Recovering: true, QueuedJobs: 3, RunningJobs: 1, FreeMemoryBytes: proto.Int64(123456),
				LoadAverage_1M: tt.loads[0], LoadAverage_5M: tt.loads[1], LoadAverage_15M: tt.loads[2],
				StorageUnavailable: true, UnreconciledReservations: 4, BackfillLimit: 5,
				QueueMode: tunapb.QueueMode_QUEUE_MODE_PAUSED,
			}
			response := httptest.NewRecorder()
			newHandler(&recordingBroker{status: status}, DefaultPort).ServeHTTP(
				response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:49783/status", nil))
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}

			decoder := json.NewDecoder(bytes.NewReader(response.Body.Bytes()))
			if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
				t.Fatalf("invalid JSON object start %v: %v", token, err)
			}
			var keys []string
			for decoder.More() {
				token, err := decoder.Token()
				if err != nil {
					t.Fatal(err)
				}
				name, ok := token.(string)
				if !ok {
					t.Fatalf("invalid JSON key %v", token)
				}
				keys = append(keys, name)
				var value json.RawMessage
				if err := decoder.Decode(&value); err != nil {
					t.Fatal(err)
				}
			}
			if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
				t.Fatalf("invalid JSON object end %v: %v", token, err)
			}

			want := map[string]any{
				"capacity": float64(6), "occupied": float64(2), "paused": true,
				"pausedAt": "2025-11-26T01:40:00Z", "recovering": true,
				"queuedJobs": float64(3), "runningJobs": float64(1),
				"headBlockedReason": "BLOCKED_REASON_UNSPECIFIED", "freeMemoryBytes": "123456",
				"storageUnavailable": true, "unreconciledReservations": float64(4), "backfillLimit": float64(5),
				"queueMode": "QUEUE_MODE_PAUSED", "queueState": "QUEUE_STATE_UNSPECIFIED",
			}
			var wantLoads []string
			for i, name := range loadNames {
				if tt.loads[i] != nil {
					want[name] = *tt.loads[i]
					wantLoads = append(wantLoads, name)
				} else if slices.Contains(keys, name) {
					t.Errorf("absent load field %q appeared in %v", name, keys)
				}
			}
			if len(wantLoads) != 0 {
				start := slices.Index(keys, wantLoads[0])
				if start < 0 || start+len(wantLoads) > len(keys) ||
					!reflect.DeepEqual(keys[start:start+len(wantLoads)], wantLoads) {
					t.Errorf("load keys %v are not consecutive in serialized fields %v", wantLoads, keys)
				}
			}
			var got map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("JSON = %v, want %v", got, want)
			}
			var decoded tunapb.BrokerStatus
			if err := protojson.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(&decoded, status) {
				t.Errorf("decoded status = %v, want %v", &decoded, status)
			}
		})
	}
}

func TestRESTReadableTimestampsAndDefaults(t *testing.T) {
	tests := []struct {
		path   string
		values map[string]any
		absent []string
	}{
		{
			path: "/health",
			values: map[string]any{
				"live": true, "ready": false, "startedAt": "2025-11-26T01:40:00Z",
			},
			absent: []string{"startedAtUnixNs", "recoveryDetail", "storageDetail"},
		},
		{
			path: "/status",
			values: map[string]any{
				"paused": false, "occupied": float64(0), "queuedJobs": float64(0),
			},
			absent: []string{"pausedAt", "loadAverage1m", "loadAverage5m", "loadAverage15m"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			newHandler(&recordingBroker{}, DefaultPort).ServeHTTP(
				response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:49783"+tt.path, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			var object map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &object); err != nil {
				t.Fatal(err)
			}
			for key, want := range tt.values {
				if got, ok := object[key]; !ok || got != want {
					t.Errorf("%s = %v (present %t), want %v", key, got, ok, want)
				}
			}
			for _, key := range tt.absent {
				if got, ok := object[key]; ok {
					t.Errorf("unknown optional %s must be absent, got %v", key, got)
				}
			}
		})
	}
}

func TestRESTRFC3339QueryTimes(t *testing.T) {
	tests := []struct {
		method string
		path   string
		query  url.Values
		want   map[string]string
	}{
		{
			method: http.MethodGet,
			path:   "/jobs",
			query: url.Values{
				"queuedAfter":  {"2026-11-26T23:30:00.123456789+01:30"},
				"queuedBefore": {"2026-11-26T22:01:00Z"},
			},
			want: map[string]string{
				"queuedAfter": "2026-11-26T22:00:00.123456789Z", "queuedBefore": "2026-11-26T22:01:00Z",
			},
		},
		{
			method: http.MethodDelete,
			path:   "/history",
			query:  url.Values{"finishedBefore": {"2026-11-26T22:00:00Z"}},
			want:   map[string]string{"finishedBefore": "2026-11-26T22:00:00Z"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			broker := &recordingBroker{}
			response := httptest.NewRecorder()
			newHandler(broker, DefaultPort).ServeHTTP(response,
				httptest.NewRequest(tt.method, "http://127.0.0.1:49783"+tt.path+"?"+tt.query.Encode(), nil))
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			data, err := protojson.Marshal(broker.request)
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]any
			if err := json.Unmarshal(data, &object); err != nil {
				t.Fatal(err)
			}
			for key, want := range tt.want {
				if object[key] != want {
					t.Errorf("%s = %v, want %s", key, object[key], want)
				}
			}
		})
	}
}

func TestRESTRejectsInvalidRFC3339Offsets(t *testing.T) {
	for _, value := range []string{
		"2026-11-26T22:00:00+24:00",
		"2026-11-26T22:00:00-03:60",
		"2026-11-26T22:00:00.1234567891Z",
		"2262-04-11T23:47:16.854775808Z",
	} {
		if _, err := parseTimestamp("queuedAfter", value); err == nil {
			t.Errorf("accepted invalid RFC 3339 value %q", value)
		}
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
		if err := detail.UnmarshalTo(&info); err == nil && info.Domain == "tuna" {
			return codes.Code(result.Code), info.Reason, info.Metadata
		}
	}
	t.Fatalf("missing tuna ErrorInfo in %s", response.Body.String())
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
		reason      tunapb.ErrorReason
		field       string
	}{
		{"unknown route", http.MethodGet, "/nope", "", "", 404, codes.NotFound, tunapb.ErrorReason_HTTP_ROUTE_NOT_FOUND, ""},
		{"runner is gRPC-only", http.MethodPost, "/runner/submit", "", "", 404, codes.NotFound, tunapb.ErrorReason_HTTP_ROUTE_NOT_FOUND, ""},
		{"method", http.MethodPost, "/status", "", "", 405, codes.Unimplemented, tunapb.ErrorReason_HTTP_METHOD_NOT_ALLOWED, ""},
		{"duplicate query", http.MethodGet, "/jobs?pageSize=1&pageSize=2", "", "", 400, codes.InvalidArgument, tunapb.ErrorReason_INVALID_ARGUMENT_FIELD, "pageSize"},
		{"unknown query", http.MethodGet, "/jobs?unknown=1", "", "", 400, codes.InvalidArgument, tunapb.ErrorReason_INVALID_ARGUMENT_FIELD, "unknown"},
		{"overflow", http.MethodDelete, "/history?finishedBefore=2262-04-11T23:47:16.854775808Z", "", "", 400, codes.InvalidArgument, tunapb.ErrorReason_INVALID_ARGUMENT_FIELD, "finishedBefore"},
		{"subnanosecond", http.MethodGet, "/jobs?queuedAfter=2026-11-26T22:00:00.1234567891Z", "", "", 400, codes.InvalidArgument, tunapb.ErrorReason_INVALID_ARGUMENT_FIELD, "queuedAfter"},
		{"invalid timestamp", http.MethodGet, "/jobs?queuedBefore=not-a-time", "", "", 400, codes.InvalidArgument, tunapb.ErrorReason_INVALID_ARGUMENT_FIELD, "queuedBefore"},
		{"path conflict", http.MethodPost, "/jobs/job-1/cancel", `{"jobId":"job-2"}`, "application/json", 400, codes.InvalidArgument, tunapb.ErrorReason_INVALID_ARGUMENT_FIELD, "jobId"},
		{"oneof conflict", http.MethodPost, "/jobs/job-1/move", `{"toFront":true,"toBack":true}`, "application/json", 400, codes.InvalidArgument, tunapb.ErrorReason_INVALID_ARGUMENT_FIELD, "body"},
		{"content type", http.MethodPost, "/jobs/job-1/cancel", `{"reason":"x"}`, "text/plain", 415, codes.InvalidArgument, tunapb.ErrorReason_HTTP_UNSUPPORTED_MEDIA_TYPE, ""},
		{"body limit", http.MethodPost, "/queue/pause", strings.Repeat("x", maxRESTBodyBytes+1), "application/json", 413, codes.ResourceExhausted, tunapb.ErrorReason_HTTP_REQUEST_TOO_LARGE, ""},
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
		request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:49783/jobs?z=1&a=1", nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		_, _, metadata := errorReason(t, response)
		if metadata["field"] != "a" {
			t.Fatalf("first invalid field = %q, want a", metadata["field"])
		}
	}
}
