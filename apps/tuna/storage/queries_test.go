package storage

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/boumenot/tuna/apps/tuna/contract"
	pb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
)

func TestFinishedOrderPaginationAndSequence(t *testing.T) {
	f := setup(t)
	var jobs []*pb.Job
	for i := 0; i < 4; i++ {
		job := f.submit(f.request(), contract.AdmissionQueued, nil).Job
		jobs = append(jobs, job)
		f.write(func(tx *Tx) error {
			return tx.exec("UPDATE jobs SET queued_at_ns=? WHERE id=?", i+1, job.JobId)
		})
	}

	for _, step := range []struct {
		index int
		at    int64
	}{{1, 100}, {2, 200}, {0, 300}, {3, 300}} {
		f.write(func(tx *Tx) error {
			tx.at = step.at
			return tx.Cancel(jobs[step.index].JobId, nil)
		})
	}

	request := &pb.ListJobsRequest{
		States: []pb.JobState{pb.JobState_JOB_STATE_FINISHED}, PageSize: 1, OrderByFinishedAt: true,
	}
	var got []string
	var sequence int64
	err := f.store.View(f.ctx, func(r *Reader) error {
		var err error
		sequence, err = r.EventSequence()
		if err != nil {
			return err
		}
		for {
			page, err := r.ListJobs(request)
			if err != nil {
				return err
			}
			if page.EventSeq != sequence {
				t.Fatalf("snapshot sequence = %d, want %d", page.EventSeq, sequence)
			}
			for _, job := range page.Jobs {
				got = append(got, job.JobId)
			}
			if page.NextPageToken == "" {
				break
			}
			request.PageToken = page.NextPageToken
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	tied := []string{jobs[0].JobId, jobs[3].JobId}
	sort.Sort(sort.Reverse(sort.StringSlice(tied)))
	want := append(tied, jobs[2].JobId, jobs[1].JobId)
	if len(got) != len(want) {
		t.Fatalf("finished order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("finished order = %v, want %v", got, want)
		}
	}

	err = f.store.View(f.ctx, func(r *Reader) error {
		defaultOrder, err := r.ListJobs(&pb.ListJobsRequest{PageSize: 1})
		if err != nil {
			return err
		}
		if len(defaultOrder.Jobs) != 1 || defaultOrder.Jobs[0].JobId != jobs[3].JobId {
			t.Fatalf("default queued order changed: %v", defaultOrder.Jobs)
		}
		request.PageToken = defaultOrder.NextPageToken
		_, err = r.ListJobs(request)
		if !errors.Is(err, ErrPageToken) {
			t.Fatalf("cursor reused with different ordering: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFinishedPageAndSequenceShareAReadSnapshot(t *testing.T) {
	f := setup(t)
	job := f.submit(f.request(), contract.AdmissionQueued, nil).Job
	writer, err := Open(f.ctx, f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	var oldSequence int64
	err = f.store.View(f.ctx, func(r *Reader) error {
		var err error
		oldSequence, err = r.EventSequence()
		if err != nil {
			return err
		}
		if _, err := writer.Write(context.Background(), func(tx *Tx) error {
			return tx.Cancel(job.JobId, nil)
		}); err != nil {
			return err
		}
		page, err := r.ListJobs(&pb.ListJobsRequest{
			States: []pb.JobState{pb.JobState_JOB_STATE_FINISHED}, OrderByFinishedAt: true,
		})
		if err != nil {
			return err
		}
		if page.EventSeq != oldSequence || len(page.Jobs) != 0 {
			t.Fatalf("page mixed snapshots: sequence=%d jobs=%v, before=%d", page.EventSeq, page.Jobs, oldSequence)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = f.store.View(f.ctx, func(r *Reader) error {
		page, err := r.ListJobs(&pb.ListJobsRequest{
			States: []pb.JobState{pb.JobState_JOB_STATE_FINISHED}, OrderByFinishedAt: true,
		})
		if err != nil {
			return err
		}
		if page.EventSeq <= oldSequence || len(page.Jobs) != 1 || page.Jobs[0].JobId != job.JobId {
			t.Fatalf("new snapshot: sequence=%d jobs=%v", page.EventSeq, page.Jobs)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFinishedPageUsesCompletionIndex(t *testing.T) {
	f := setup(t)
	err := f.store.View(f.ctx, func(r *Reader) error {
		rows, err := r.q.QueryContext(r.ctx, `EXPLAIN QUERY PLAN SELECT id FROM `+finishedJobsSource+`
			WHERE state='finished' AND (finished_at_ns,id) < (?,?)
			ORDER BY finished_at_ns DESC,id DESC LIMIT ?`, 100, "id", 51)
		if err != nil {
			return err
		}
		defer rows.Close()
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				return err
			}
			plan = append(plan, detail)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		text := strings.Join(plan, "; ")
		if !strings.Contains(text, "jobs_finished") || strings.Contains(text, "TEMP B-TREE") {
			t.Fatalf("completion query is not index-ordered: %s", text)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
