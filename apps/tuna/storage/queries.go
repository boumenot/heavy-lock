package storage

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"github.com/boumenot/tuna/apps/tuna/contract"
	pb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var ErrPageToken = errors.New("invalid page token or changed filters")

type pageKey struct {
	At      int64  `json:"at"`
	ID      string `json:"id"`
	Filters string `json:"filters"`
}

func (r *Reader) EventSequence() (int64, error) {
	var seq int64
	err := r.scan("SELECT last_event_seq FROM broker_state WHERE id=1", nil, &seq)
	return seq, err
}

// ListJobs uses a keyset cursor tied to the exact filters, never an offset.
func (r *Reader) ListJobs(req *pb.ListJobsRequest) (*pb.ListJobsResponse, error) {
	filters := proto.Clone(req).(*pb.ListJobsRequest)
	filters.PageToken, filters.PageSize = "", 0
	filterJSON, err := protojson.Marshal(filters)
	if err != nil {
		return nil, err
	}
	var key pageKey
	if req.PageToken != "" {
		data, err := base64.RawURLEncoding.DecodeString(req.PageToken)
		if err != nil || json.Unmarshal(data, &key) != nil || key.ID == "" || key.Filters != string(filterJSON) {
			return nil, ErrPageToken
		}
	}
	query := "SELECT id FROM jobs WHERE 1=1"
	var args []any
	if len(req.States) > 0 {
		query += " AND state IN ("
		for i, state := range req.States {
			if i > 0 {
				query += ","
			}
			query += "?"
			if state < pb.JobState_JOB_STATE_QUEUED || state > pb.JobState_JOB_STATE_FINISHED {
				return nil, errors.New("invalid job state")
			}
			name := strings.ToLower(strings.TrimPrefix(state.String(), "JOB_STATE_"))
			args = append(args, name)
		}
		query += ")"
	}
	for _, filter := range []struct {
		column string
		value  *string
	}{
		{"repository", req.Repository}, {"client_name", req.ClientName},
	} {
		if filter.value != nil {
			query += " AND " + filter.column + "=?"
			args = append(args, *filter.value)
		}
	}
	if req.QueuedAfter != nil {
		at, err := contract.UnixNano(req.QueuedAfter)
		if err != nil {
			return nil, err
		}
		query += " AND queued_at_ns>=?"
		args = append(args, at)
	}
	if req.QueuedBefore != nil {
		at, err := contract.UnixNano(req.QueuedBefore)
		if err != nil {
			return nil, err
		}
		query += " AND queued_at_ns<?"
		args = append(args, at)
	}
	if req.PageToken != "" {
		query += " AND (queued_at_ns<? OR (queued_at_ns=? AND id<?))"
		args = append(args, key.At, key.At, key.ID)
	}
	size := req.PageSize
	if size == 0 {
		size = 50
	}
	if size > 500 {
		return nil, ErrPageToken
	}
	query += " ORDER BY queued_at_ns DESC,id DESC LIMIT ?"
	args = append(args, size+1)
	ids, err := r.ids(query, args...)
	if err != nil {
		return nil, err
	}
	result := &pb.ListJobsResponse{}
	for i, id := range ids {
		if uint32(i) == size {
			break
		}
		job, err := r.Job(id)
		if err != nil {
			return nil, err
		}
		result.Jobs = append(result.Jobs, job)
	}
	if uint32(len(ids)) > size {
		last := result.Jobs[len(result.Jobs)-1]
		at, err := contract.UnixNano(last.QueuedAt)
		if err != nil {
			return nil, err
		}
		data, err := json.Marshal(pageKey{At: at, ID: last.JobId, Filters: string(filterJSON)})
		if err != nil {
			return nil, err
		}
		result.NextPageToken = base64.RawURLEncoding.EncodeToString(data)
	}
	return result, nil
}
