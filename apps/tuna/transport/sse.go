package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/boumenot/tuna/apps/tuna/contract"
	pb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

const sseWriteTimeout = 10 * time.Second

func serveEvents(w http.ResponseWriter, r *http.Request, broker Broker, epoch string, epochErr error) {
	query, err := parseQuery(r, map[string]bool{"afterEventId": false})
	if err != nil {
		writeRESTError(w, err, 0)
		return
	}
	if epochErr != nil {
		writeRESTError(w, contract.Error(codes.Internal, pb.ErrorReason_INTERNAL_ERROR,
			"cannot generate broker stream incarnation: "+epochErr.Error(), nil), 0)
		return
	}
	cursor := ""
	if values := query["afterEventId"]; len(values) != 0 {
		if values[0] == "" {
			writeRESTError(w, invalidField("afterEventId", "cursor must not be empty"), 0)
			return
		}
		cursor = values[0]
	}
	headers := r.Header.Values("Last-Event-ID")
	if len(headers) > 1 {
		writeRESTError(w, invalidField("Last-Event-ID", "header must occur once"), 0)
		return
	}
	if len(headers) == 1 {
		cursor = headers[0]
	}
	var after int64
	if cursor != "" {
		parts := strings.Split(cursor, ":")
		if len(parts) != 2 || !hexEpoch(parts[0]) || !decimal(parts[1]) {
			writeRESTError(w, invalidField("cursor", "expected an SSE event ID"), 0)
			return
		}
		after, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			writeRESTError(w, invalidField("cursor", "expected an SSE event ID"), 0)
			return
		}
		if parts[0] != epoch {
			writeRESTError(w, contract.Error(codes.OutOfRange, pb.ErrorReason_EVENTS_EXPIRED,
				"broker restarted; discard the cursor and request a fresh snapshot", nil), 0)
			return
		}
	}
	stream := &sseStream{ctx: r.Context(), w: w, epoch: epoch}
	err = broker.WatchEvents(&pb.WatchEventsRequest{AfterEventSeq: after}, stream)
	if r.Context().Err() != nil {
		return
	}
	if stream.writeErr != nil && stream.started {
		log.Printf("tuna: write SSE event: %v", stream.writeErr)
		return
	}
	if err == nil {
		err = contract.Error(codes.Unavailable, pb.ErrorReason_INTERNAL_ERROR,
			"broker event stream ended unexpectedly", nil)
	}
	if !stream.started {
		writeRESTError(w, err, 0)
		return
	}
	data, marshalErr := (protojson.MarshalOptions{EmitDefaultValues: true}).Marshal(status.Convert(contract.NormalizeError(err)).Proto())
	if marshalErr != nil {
		log.Printf("tuna: encode SSE stream error: %v", marshalErr)
		return
	}
	if writeErr := stream.writeFrame("stream_error", "", data); writeErr != nil {
		log.Printf("tuna: write SSE stream error: %v", writeErr)
	}
}

func hexEpoch(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func decimal(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

type sseStream struct {
	ctx      context.Context
	w        http.ResponseWriter
	epoch    string
	started  bool
	writeErr error
}

func (s *sseStream) Context() context.Context   { return s.ctx }
func (*sseStream) SetHeader(metadata.MD) error  { return nil }
func (*sseStream) SendHeader(metadata.MD) error { return nil }
func (*sseStream) SetTrailer(metadata.MD)       {}
func (*sseStream) RecvMsg(any) error            { return io.EOF }
func (s *sseStream) SendMsg(message any) error {
	event, ok := message.(*pb.BrokerEvent)
	if !ok {
		return errors.New("SSE stream received a non-event message")
	}
	return s.Send(event)
}

func (s *sseStream) Send(event *pb.BrokerEvent) error {
	if event == nil || event.EventSeq < 0 {
		return errors.New("broker returned an invalid event sequence")
	}
	var name string
	switch event.Event.(type) {
	case *pb.BrokerEvent_Snapshot:
		name = "snapshot"
	case *pb.BrokerEvent_JobChanged:
		name = "job_changed"
	case *pb.BrokerEvent_QueueChanged:
		name = "queue_changed"
	case *pb.BrokerEvent_StatusChanged:
		name = "status_changed"
	case *pb.BrokerEvent_JobDeleted:
		name = "job_deleted"
	case *pb.BrokerEvent_Heartbeat:
		name = "heartbeat"
	default:
		return errors.New("broker returned an unknown or empty event")
	}
	if event.EventSeq == 0 && name != "snapshot" && name != "heartbeat" {
		return errors.New("broker returned an invalid event sequence")
	}
	data, err := (protojson.MarshalOptions{EmitDefaultValues: true}).Marshal(event)
	if err != nil {
		return fmt.Errorf("encode broker event: %w", err)
	}
	err = s.writeFrame(name, fmt.Sprintf("%s:%d", s.epoch, event.EventSeq), data)
	if err != nil {
		s.writeErr = err
	}
	return err
}

func (s *sseStream) writeFrame(name, id string, data []byte) error {
	controller := http.NewResponseController(s.w)
	if err := controller.SetWriteDeadline(time.Now().Add(sseWriteTimeout)); err != nil {
		return fmt.Errorf("set SSE write deadline: %w", err)
	}
	if !s.started {
		s.w.Header().Set("Content-Type", "text/event-stream")
		s.w.Header().Set("Cache-Control", "no-store")
	}
	var frame strings.Builder
	fmt.Fprintf(&frame, "event: %s\n", name)
	if id != "" {
		fmt.Fprintf(&frame, "id: %s\n", id)
	}
	fmt.Fprintf(&frame, "data: %s\n\n", data)
	s.started = true
	frameText := frame.String()
	written, err := io.WriteString(s.w, frameText)
	if err != nil {
		return fmt.Errorf("write SSE frame: %w", err)
	}
	if written != len(frameText) {
		return fmt.Errorf("write SSE frame: %w", io.ErrShortWrite)
	}
	if err := controller.Flush(); err != nil {
		return fmt.Errorf("flush SSE frame: %w", err)
	}
	if err := controller.SetWriteDeadline(time.Time{}); err != nil {
		return fmt.Errorf("clear SSE write deadline: %w", err)
	}
	return nil
}
