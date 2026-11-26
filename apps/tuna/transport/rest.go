package transport

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/boumenot/heavy-lock/apps/tuna/contract"
	heavylockv2 "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const maxRESTBodyBytes = 1 << 20

type endpoint struct {
	allowQuery bool
	call       func(*http.Request) (proto.Message, error)
}

func newRESTHandler(broker Broker) http.Handler {
	mux := http.NewServeMux()
	register := func(path string, methods map[string]endpoint) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			allow := make([]string, 0, len(methods)+1)
			for method := range methods {
				allow = append(allow, method)
			}
			sort.Strings(allow)
			w.Header().Set("Allow", strings.Join(append(append([]string{}, allow...), http.MethodOptions), ", "))
			if r.Method == http.MethodOptions {
				handlePreflight(w, r, methods, allow)
				return
			}
			handler, ok := methods[r.Method]
			if !ok {
				writeRESTError(w, contract.Error(codes.Unimplemented, heavylockv2.ErrorReason_HTTP_METHOD_NOT_ALLOWED,
					"method is not supported on this route", nil), http.StatusMethodNotAllowed)
				return
			}
			if !handler.allowQuery && r.URL.RawQuery != "" {
				writeRESTError(w, invalidField("query", "this route does not accept query parameters"), 0)
				return
			}
			if r.Method == http.MethodGet || r.Method == http.MethodDelete {
				if err := requireEmptyBody(r); err != nil {
					writeRESTError(w, err, 0)
					return
				}
			}
			result, err := handler.call(r)
			if err != nil {
				writeRESTError(w, err, 0)
				return
			}
			if result == nil || !result.ProtoReflect().IsValid() {
				writeRESTError(w, contract.Error(codes.Internal, heavylockv2.ErrorReason_INTERNAL_ERROR,
					"broker returned an empty response", nil), 0)
				return
			}
			data, err := protojson.Marshal(result)
			if err != nil {
				writeRESTError(w, contract.Error(codes.Internal, heavylockv2.ErrorReason_INTERNAL_ERROR,
					"cannot encode broker response: "+err.Error(), nil), 0)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if _, err := w.Write(data); err != nil {
				log.Printf("tuna: write REST response: %v", err)
			}
		})
	}

	register("/v1/health", map[string]endpoint{http.MethodGet: {call: func(r *http.Request) (proto.Message, error) {
		return broker.GetHealth(r.Context(), &heavylockv2.GetHealthRequest{})
	}}})
	register("/v1/status", map[string]endpoint{http.MethodGet: {call: func(r *http.Request) (proto.Message, error) {
		return broker.GetStatus(r.Context(), &heavylockv2.GetStatusRequest{})
	}}})
	register("/v1/queue", map[string]endpoint{http.MethodGet: {call: func(r *http.Request) (proto.Message, error) {
		return broker.GetQueue(r.Context(), &heavylockv2.GetQueueRequest{})
	}}})
	register("/v1/jobs", map[string]endpoint{http.MethodGet: {allowQuery: true, call: func(r *http.Request) (proto.Message, error) {
		request, err := listJobsRequest(r)
		if err != nil {
			return nil, err
		}
		return broker.ListJobs(r.Context(), request)
	}}})
	register("/v1/jobs/{job_id}", map[string]endpoint{
		http.MethodGet: {call: func(r *http.Request) (proto.Message, error) {
			return broker.GetJob(r.Context(), &heavylockv2.GetJobRequest{JobId: r.PathValue("job_id")})
		}},
		http.MethodDelete: {call: func(r *http.Request) (proto.Message, error) {
			return broker.DeleteJob(r.Context(), &heavylockv2.DeleteJobRequest{JobId: r.PathValue("job_id")})
		}},
	})
	register("/v1/submissions/{idempotency_key}", map[string]endpoint{http.MethodGet: {call: func(r *http.Request) (proto.Message, error) {
		return broker.GetSubmission(r.Context(), &heavylockv2.GetSubmissionRequest{IdempotencyKey: r.PathValue("idempotency_key")})
	}}})
	register("/v1/jobs/{job_id}/cancel", map[string]endpoint{http.MethodPost: {call: func(r *http.Request) (proto.Message, error) {
		request := &heavylockv2.CancelJobRequest{}
		if err := decodeBody(r, request, "reason"); err != nil {
			return nil, err
		}
		request.JobId = r.PathValue("job_id")
		return broker.CancelJob(r.Context(), request)
	}}})
	register("/v1/jobs/{job_id}/move", map[string]endpoint{http.MethodPost: {call: func(r *http.Request) (proto.Message, error) {
		request := &heavylockv2.MoveJobRequest{}
		if err := decodeBody(r, request, "toFront", "toBack", "beforeJobId", "afterJobId"); err != nil {
			return nil, err
		}
		request.JobId = r.PathValue("job_id")
		return broker.MoveJob(r.Context(), request)
	}}})
	register("/v1/jobs/{job_id}/force-start", map[string]endpoint{http.MethodPost: {call: func(r *http.Request) (proto.Message, error) {
		request := &heavylockv2.ForceStartJobRequest{}
		if err := decodeBody(r, request); err != nil {
			return nil, err
		}
		request.JobId = r.PathValue("job_id")
		return broker.ForceStartJob(r.Context(), request)
	}}})
	register("/v1/queue/pause", map[string]endpoint{http.MethodPost: {call: func(r *http.Request) (proto.Message, error) {
		request := &heavylockv2.PauseQueueRequest{}
		if err := decodeBody(r, request); err != nil {
			return nil, err
		}
		return broker.PauseQueue(r.Context(), request)
	}}})
	register("/v1/queue/resume", map[string]endpoint{http.MethodPost: {call: func(r *http.Request) (proto.Message, error) {
		request := &heavylockv2.ResumeQueueRequest{}
		if err := decodeBody(r, request); err != nil {
			return nil, err
		}
		return broker.ResumeQueue(r.Context(), request)
	}}})
	register("/v1/history", map[string]endpoint{http.MethodDelete: {allowQuery: true, call: func(r *http.Request) (proto.Message, error) {
		query, err := parseQuery(r, map[string]bool{"finishedBeforeUnixNs": false})
		if err != nil {
			return nil, err
		}
		value, ok := query["finishedBeforeUnixNs"]
		if !ok {
			return nil, invalidField("finishedBeforeUnixNs", "finishedBeforeUnixNs is required")
		}
		cutoff, err := strconv.ParseInt(value[0], 10, 64)
		if err != nil {
			return nil, invalidField("finishedBeforeUnixNs", "finishedBeforeUnixNs must be a signed 64-bit integer")
		}
		return broker.DeleteHistory(r.Context(), &heavylockv2.DeleteHistoryRequest{FinishedBeforeUnixNs: cutoff})
	}}})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeRESTError(w, contract.Error(codes.NotFound, heavylockv2.ErrorReason_HTTP_ROUTE_NOT_FOUND,
			"REST route not found", nil), http.StatusNotFound)
	})
	return mux
}

func handlePreflight(w http.ResponseWriter, r *http.Request, methods map[string]endpoint, allow []string) {
	w.Header().Add("Vary", "Access-Control-Request-Method")
	w.Header().Add("Vary", "Access-Control-Request-Headers")
	w.Header().Add("Vary", "Access-Control-Request-Private-Network")
	requested := r.Header.Get("Access-Control-Request-Method")
	if requested == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Header.Get("Origin") == "" {
		writeRESTError(w, invalidField("Origin", "preflight requires an Origin"), 0)
		return
	}
	if _, ok := methods[requested]; !ok {
		writeRESTError(w, contract.Error(codes.Unimplemented, heavylockv2.ErrorReason_HTTP_METHOD_NOT_ALLOWED,
			"preflight method is not supported on this route", nil), http.StatusMethodNotAllowed)
		return
	}
	for _, header := range strings.Split(r.Header.Get("Access-Control-Request-Headers"), ",") {
		header = strings.TrimSpace(header)
		if header != "" && !strings.EqualFold(header, "Content-Type") && !strings.EqualFold(header, "Accept") {
			writeRESTError(w, invalidField("Access-Control-Request-Headers", "preflight header is not supported"), 0)
			return
		}
	}
	w.Header().Set("Access-Control-Allow-Methods", strings.Join(allow, ", "))
	w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type")
	if privateNetwork := r.Header.Get("Access-Control-Request-Private-Network"); privateNetwork != "" {
		if !strings.EqualFold(privateNetwork, "true") {
			writeRESTError(w, invalidField("Access-Control-Request-Private-Network", "invalid private-network preflight"), 0)
			return
		}
		w.Header().Set("Access-Control-Allow-Private-Network", "true")
	}
	w.WriteHeader(http.StatusNoContent)
}

func listJobsRequest(r *http.Request) (*heavylockv2.ListJobsRequest, error) {
	query, err := parseQuery(r, map[string]bool{
		"states": true, "repository": false, "clientName": false, "queuedAfterUnixNs": false,
		"queuedBeforeUnixNs": false, "pageSize": false, "pageToken": false,
	})
	if err != nil {
		return nil, err
	}
	request := &heavylockv2.ListJobsRequest{}
	for _, value := range query["states"] {
		number, ok := heavylockv2.JobState_value[value]
		if !ok {
			return nil, invalidField("states", "states must contain JobState enum names")
		}
		request.States = append(request.States, heavylockv2.JobState(number))
	}
	if value, ok := query["repository"]; ok {
		request.Repository = proto.String(value[0])
	}
	if value, ok := query["clientName"]; ok {
		request.ClientName = proto.String(value[0])
	}
	if value, ok := query["queuedAfterUnixNs"]; ok {
		number, err := strconv.ParseInt(value[0], 10, 64)
		if err != nil {
			return nil, invalidField("queuedAfterUnixNs", "queuedAfterUnixNs must be a signed 64-bit integer")
		}
		request.QueuedAfterUnixNs = proto.Int64(number)
	}
	if value, ok := query["queuedBeforeUnixNs"]; ok {
		number, err := strconv.ParseInt(value[0], 10, 64)
		if err != nil {
			return nil, invalidField("queuedBeforeUnixNs", "queuedBeforeUnixNs must be a signed 64-bit integer")
		}
		request.QueuedBeforeUnixNs = proto.Int64(number)
	}
	if value, ok := query["pageSize"]; ok {
		number, err := strconv.ParseUint(value[0], 10, 32)
		if err != nil {
			return nil, invalidField("pageSize", "pageSize must be an unsigned 32-bit integer")
		}
		request.PageSize = uint32(number)
	}
	if value, ok := query["pageToken"]; ok {
		request.PageToken = value[0]
	}
	return request, nil
}

func parseQuery(r *http.Request, allowed map[string]bool) (url.Values, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, invalidField("query", "malformed query parameters")
	}
	keys := make([]string, 0, len(query))
	for key := range query {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		values := query[key]
		repeated, ok := allowed[key]
		if !ok {
			return nil, invalidField(key, "unknown query parameter")
		}
		if !repeated && len(values) != 1 {
			return nil, invalidField(key, "query parameter must occur once")
		}
	}
	return query, nil
}

func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxRESTBodyBytes+1))
	if err != nil {
		return nil, invalidField("body", "cannot read request body: "+err.Error())
	}
	if len(data) > maxRESTBodyBytes {
		return nil, contract.Error(codes.ResourceExhausted, heavylockv2.ErrorReason_HTTP_REQUEST_TOO_LARGE,
			"REST request body exceeds 1 MiB", nil)
	}
	return bytes.TrimSpace(data), nil
}

func requireEmptyBody(r *http.Request) error {
	data, err := readBody(r)
	if err != nil {
		return err
	}
	if len(data) != 0 {
		return invalidField("body", "this route does not accept a request body")
	}
	return nil
}

func decodeBody(r *http.Request, message proto.Message, allowed ...string) error {
	data, err := readBody(r)
	if err != nil || len(data) == 0 {
		return err
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return contract.Error(codes.InvalidArgument, heavylockv2.ErrorReason_HTTP_UNSUPPORTED_MEDIA_TYPE,
			"REST request body must use application/json", nil)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return invalidField("body", "REST request body must be a JSON object")
	}
	fields := make([]string, 0, len(object))
	for field := range object {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	for _, field := range fields {
		found := false
		for _, name := range allowed {
			if field == name {
				found = true
				break
			}
		}
		if !found {
			return invalidField(field, "field is not accepted in this request body")
		}
	}
	if err := protojson.Unmarshal(data, message); err != nil {
		return invalidField("body", "invalid protobuf JSON body: "+err.Error())
	}
	return nil
}

func invalidField(field, message string) error {
	return contract.Error(codes.InvalidArgument, heavylockv2.ErrorReason_INVALID_ARGUMENT_FIELD, message,
		map[string]string{"field": field})
}

func writeRESTError(w http.ResponseWriter, err error, httpStatus int) {
	result := status.Convert(contract.NormalizeError(err))
	data, marshalErr := protojson.Marshal(result.Proto())
	if marshalErr != nil {
		http.Error(w, "could not encode REST error: "+marshalErr.Error(), http.StatusInternalServerError)
		return
	}
	if httpStatus == 0 {
		httpStatus = statusHTTPCode(result.Code())
		for _, detail := range result.Details() {
			if info, ok := detail.(*errdetails.ErrorInfo); ok {
				switch info.Reason {
				case heavylockv2.ErrorReason_HTTP_UNSUPPORTED_MEDIA_TYPE.String():
					httpStatus = http.StatusUnsupportedMediaType
				case heavylockv2.ErrorReason_HTTP_REQUEST_TOO_LARGE.String():
					httpStatus = http.StatusRequestEntityTooLarge
				}
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	if _, err := w.Write(data); err != nil {
		log.Printf("tuna: write REST error: %v", err)
	}
}

func statusHTTPCode(code codes.Code) int {
	switch code {
	case codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange:
		return http.StatusBadRequest
	case codes.Unauthenticated:
		return http.StatusUnauthorized
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.NotFound:
		return http.StatusNotFound
	case codes.AlreadyExists, codes.Aborted:
		return http.StatusConflict
	case codes.ResourceExhausted:
		return http.StatusTooManyRequests
	case codes.Unimplemented:
		return http.StatusNotImplemented
	case codes.Unavailable:
		return http.StatusServiceUnavailable
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout
	case codes.Canceled:
		return http.StatusRequestTimeout
	default:
		return http.StatusInternalServerError
	}
}
