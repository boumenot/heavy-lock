package storage

import (
	"errors"

	"github.com/boumenot/heavy-lock/apps/tuna/contract"
	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"google.golang.org/protobuf/proto"
)

func (tx *Tx) event(event *pb.BrokerEvent, kind string, jobID any) error {
	if tx.ProjectEvent != nil {
		if err := tx.ProjectEvent(&tx.Reader, event); err != nil {
			return err
		}
	}
	if !matchesEvent(event, kind, jobID) {
		return errors.New("event projection changed the durable subject or kind")
	}
	if err := tx.exec("UPDATE broker_state SET last_event_seq=last_event_seq+1 WHERE id=1"); err != nil {
		return err
	}
	if err := tx.scan("SELECT last_event_seq FROM broker_state WHERE id=1", nil, &event.EventSeq); err != nil {
		return err
	}
	event.At = contract.Timestamp(tx.at)
	payload, err := proto.Marshal(event)
	if err != nil {
		return err
	}
	if err := tx.exec("INSERT INTO events(seq,at_ns,job_id,kind,payload) VALUES (?,?,?,?,?)",
		event.EventSeq, tx.at, jobID, kind, payload); err != nil {
		return err
	}
	tx.events = append(tx.events, event)
	return nil
}

func matchesEvent(event *pb.BrokerEvent, kind string, jobID any) bool {
	switch kind {
	case "job_changed":
		id, ok := jobID.(string)
		return ok && event.GetJobChanged() != nil && event.GetJobChanged().JobId == id
	case "queue_changed":
		return jobID == nil && event.GetQueueChanged() != nil
	case "status_changed":
		return jobID == nil && event.GetStatusChanged() != nil
	case "job_deleted":
		id, ok := jobID.(string)
		return ok && event.GetJobDeleted() == id
	}
	return false
}

func (tx *Tx) jobChanged(id string) error {
	j, err := tx.Job(id)
	if err != nil {
		return err
	}
	return tx.event(&pb.BrokerEvent{Event: &pb.BrokerEvent_JobChanged{JobChanged: j}}, "job_changed", id)
}

func (tx *Tx) queueChanged() error {
	q, err := tx.Queue()
	if err != nil {
		return err
	}
	return tx.event(&pb.BrokerEvent{Event: &pb.BrokerEvent_QueueChanged{QueueChanged: q}}, "queue_changed", nil)
}

func (tx *Tx) statusChanged() error {
	s, err := tx.Status()
	if err != nil {
		return err
	}
	return tx.event(&pb.BrokerEvent{Event: &pb.BrokerEvent_StatusChanged{StatusChanged: s}}, "status_changed", nil)
}

// Events reads a consistent replay window. A zero cursor is a request for a
// broker snapshot, not replay; the broker constructs it in this same View.
func (r *Reader) Events(after int64, limit int) ([]*pb.BrokerEvent, error) {
	if limit < 1 || limit > 100000 {
		return nil, errors.New("event limit must be 1..100000")
	}
	var floor, last int64
	if err := r.scan("SELECT event_floor_seq,last_event_seq FROM broker_state WHERE id=1", nil, &floor, &last); err != nil {
		return nil, err
	}
	if after < floor || after > last || after < 1 {
		return nil, ErrEventsExpired
	}
	rows, err := r.q.QueryContext(r.ctx, "SELECT seq,at_ns,kind,job_id,payload FROM events WHERE seq>? ORDER BY seq LIMIT ?", after, limit)
	if err != nil {
		r.err = errors.Join(r.err, err)
		return nil, err
	}
	var events []*pb.BrokerEvent
	expected := after
	for rows.Next() {
		var seq, at int64
		var kind string
		var jobID *string
		var payload []byte
		if err = rows.Scan(&seq, &at, &kind, &jobID, &payload); err != nil {
			break
		}
		expected++
		if seq != expected {
			err = errors.New("replayable event sequence has an unrecorded gap")
			break
		}
		event := &pb.BrokerEvent{}
		if err = proto.Unmarshal(payload, event); err != nil {
			break
		}
		if err = restoreLegacyEvent(event.ProtoReflect()); err != nil {
			break
		}
		eventAt, timeErr := contract.UnixNano(event.At)
		var subject any
		if jobID != nil {
			subject = *jobID
		}
		if timeErr != nil || event.EventSeq != seq || eventAt != at || !matchesEvent(event, kind, subject) {
			err = errors.New("event payload disagrees with stored sequence, timestamp or subject")
			break
		}
		events = append(events, event)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err == nil && len(events) < limit && expected < last {
		err = errors.New("replayable event sequence has an unrecorded gap")
	}
	if err != nil {
		r.err = errors.Join(r.err, err)
		return nil, err
	}
	return events, nil
}

// PruneEvents removes events older than cutoff or outside the newest keep
// sequences. Job history, active state and permanent keys are never pruned.
// The caller supplies the configured retention (initially 24h / 100000).
func (tx *Tx) PruneEvents(cutoff int64, keep int64) error {
	if keep < 1 {
		return errors.New("event retention count must be positive")
	}
	var through *int64
	if err := tx.scan(`SELECT max(seq) FROM events WHERE at_ns<? OR
		seq <= (SELECT last_event_seq-? FROM broker_state WHERE id=1)`, []any{cutoff, keep}, &through); err != nil {
		return err
	}
	if through == nil {
		return nil
	}
	if err := tx.exec("DELETE FROM events WHERE at_ns<? OR seq <= (SELECT last_event_seq-? FROM broker_state WHERE id=1)", cutoff, keep); err != nil {
		return err
	}
	return tx.exec("UPDATE broker_state SET event_floor_seq=max(event_floor_seq,?) WHERE id=1", *through)
}

func (tx *Tx) DeleteJob(id string) error {
	j, err := tx.Job(id)
	if err != nil {
		return err
	}
	if j.State != pb.JobState_JOB_STATE_FINISHED {
		return ErrTransition
	}
	var unsafe bool
	if err := tx.scan(`SELECT EXISTS(SELECT 1 FROM reservations WHERE job_id=? AND released_at_ns IS NULL)
		OR EXISTS(SELECT 1 FROM job_processes WHERE job_id=? AND exited_verified_at_ns IS NULL)`,
		[]any{id, id}, &unsafe); err != nil {
		return err
	}
	if unsafe {
		return ErrUnreconciled
	}
	var through *int64
	if err := tx.scan("SELECT max(seq) FROM events WHERE job_id=? OR kind='queue_changed'", []any{id}, &through); err != nil {
		return err
	}
	if through != nil {
		if err := tx.exec("DELETE FROM events WHERE job_id=? OR kind='queue_changed'", id); err != nil {
			return err
		}
		if err := tx.exec("UPDATE broker_state SET event_floor_seq=max(event_floor_seq,?) WHERE id=1", *through); err != nil {
			return err
		}
	}
	for _, statement := range []string{
		"DELETE FROM reservations WHERE job_id=?",
		"DELETE FROM job_processes WHERE job_id=?",
		"DELETE FROM jobs WHERE id=?",
	} {
		if err := tx.exec(statement, id); err != nil {
			return err
		}
	}
	return tx.event(&pb.BrokerEvent{Event: &pb.BrokerEvent_JobDeleted{JobDeleted: id}}, "job_deleted", id)
}

func (tx *Tx) DeleteHistory(cutoff int64) (deleted, skipped uint32, err error) {
	ids, err := tx.ids("SELECT id FROM jobs WHERE finished_at_ns<? OR state!='finished'", cutoff)
	if err != nil {
		return 0, 0, err
	}
	for _, id := range ids {
		err := tx.DeleteJob(id)
		if errors.Is(err, ErrTransition) || errors.Is(err, ErrUnreconciled) {
			skipped++
		} else if err != nil {
			return 0, 0, err
		} else {
			deleted++
		}
	}
	return deleted, skipped, nil
}
