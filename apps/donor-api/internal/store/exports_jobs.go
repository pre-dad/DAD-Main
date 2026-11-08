package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"slices"
	"time"

	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	// exportRetention is how long a finished file, or the record of a failed
	// one, stays available before the worker purges it.
	exportRetention = 24 * time.Hour
	// maxOpenExports bounds the files one staff member can have in progress.
	maxOpenExports = 3
	// maxExportBytes caps a file's stored, compressed size. Postgres keeps it,
	// so an unbounded export would be unbounded database growth.
	maxExportBytes = 50 << 20
)

const (
	exportRunning  = "running"
	exportComplete = "complete"
	exportFailed   = "failed"

	exportTooLarge  = "too_large"
	exportForbidden = "forbidden"
	exportError     = "error"
)

// ExportJob is a requested export and how far the worker has got.
type ExportJob struct {
	ID          string     `json:"id"`
	Dataset     string     `json:"dataset"`
	State       string     `json:"state"`
	Failure     string     `json:"failure,omitempty"`
	RequestedAt time.Time  `json:"requested_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	ExpiresAt   time.Time  `json:"expires_at"`
	RowCount    int64      `json:"row_count"`
}

type ExportJobs struct {
	Jobs []ExportJob `json:"jobs"`
}

func exportJobView(job queries.DonorExportJob) ExportJob {
	return ExportJob{
		ID:          job.ID.String(),
		Dataset:     job.Dataset,
		State:       job.State,
		Failure:     job.Failure.String,
		RequestedAt: job.RequestedAt.Time,
		CompletedAt: timePointer(job.CompletedAt),
		ExpiresAt:   job.ExpiresAt.Time,
		RowCount:    job.RowCount,
	}
}

// RequestExport queues an export of name for the signed-in staff member and
// audits the request. Asking again for a file still being built answers with
// that job instead of queueing a second.
func (s *StaffService) RequestExport(ctx context.Context, token, name string) (ExportJob, error) {
	dataset, ok := exportDatasets[name]
	if !ok {
		return ExportJob{}, ErrStaffInput
	}
	return staffWrite(ctx, s, token, dataset.permission, func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (ExportJob, error) {
		q := queries.New(tx)
		open, err := q.OpenExportJob(ctx, queries.OpenExportJobParams{ActorID: actor.ID, Dataset: name})
		if err == nil {
			return exportJobView(open), nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return ExportJob{}, err
		}
		count, err := q.CountOpenExportJobs(ctx, actor.ID)
		if err != nil {
			return ExportJob{}, err
		}
		if count >= maxOpenExports {
			return ExportJob{}, ErrStaffConflict
		}
		id, err := NewID()
		if err != nil {
			return ExportJob{}, err
		}
		job, err := q.InsertExportJob(ctx, queries.InsertExportJobParams{
			ID: id, ActorID: actor.ID, Dataset: name, RequestedAt: timestamp(now), ExpiresAt: timestamp(now.Add(exportRetention)),
		})
		if err != nil {
			return ExportJob{}, err
		}
		if err = auditStaff(ctx, q, actor.ID, "data.export_started", name, map[string]any{"export_id": id.String()}, now); err != nil {
			return ExportJob{}, err
		}
		return exportJobView(job), nil
	})
}

func permitted(role, dataset string) bool {
	return slices.Contains(staffPermissions(role), exportDatasets[dataset].permission)
}

// ownExport loads the signed-in staff member's own export, which they must
// still be permitted to read. Another member's export is not found.
func ownExport(
	ctx context.Context,
	q *queries.Queries,
	token string,
	id pgtype.UUID,
	now time.Time,
) (queries.DonorStaff, queries.DonorExportJob, error) {
	actor, err := requireStaff(ctx, q, token, "", now)
	if err != nil {
		return actor, queries.DonorExportJob{}, err
	}
	job, err := q.ExportJobForActor(ctx, queries.ExportJobForActorParams{ID: id, ActorID: actor.ID, AsOf: timestamp(now)})
	if err != nil {
		return actor, job, err
	}
	if !permitted(actor.Role, job.Dataset) {
		return actor, job, ErrStaffForbidden
	}
	return actor, job, nil
}

// ExportJob reports the progress of one of the staff member's exports.
func (s *StaffService) ExportJob(ctx context.Context, token string, id pgtype.UUID) (ExportJob, error) {
	return staffRead(ctx, s, token, "", func(tx pgx.Tx, _ queries.DonorStaff, now time.Time) (ExportJob, error) {
		_, job, err := ownExport(ctx, queries.New(tx), token, id, now)
		return exportJobView(job), err
	})
}

// ExportJobs lists the staff member's exports that have not yet expired, newest
// first, leaving out datasets their role no longer permits.
func (s *StaffService) ExportJobs(ctx context.Context, token string) (ExportJobs, error) {
	return staffRead(ctx, s, token, "", func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (ExportJobs, error) {
		jobs, err := queries.New(tx).ListExportJobs(ctx, queries.ListExportJobsParams{ActorID: actor.ID, AsOf: timestamp(now)})
		result := ExportJobs{Jobs: []ExportJob{}}
		for _, job := range jobs {
			if permitted(actor.Role, job.Dataset) {
				result.Jobs = append(result.Jobs, exportJobView(job))
			}
		}
		return result, err
	})
}

// ExportDownload is a finished export cleared for download by its requester.
type ExportDownload struct {
	Dataset string
	db      queries.DBTX
	job     queries.DonorExportJob
}

// OpenExport clears the staff member to download a finished export and audits
// that they did. Only the requester may download, and only while their role
// still permits the dataset.
func (s *StaffService) OpenExport(ctx context.Context, token string, id pgtype.UUID) (ExportDownload, error) {
	return staffRead(ctx, s, token, "", func(tx pgx.Tx, _ queries.DonorStaff, now time.Time) (ExportDownload, error) {
		q := queries.New(tx)
		actor, job, err := ownExport(ctx, q, token, id, now)
		if err != nil {
			return ExportDownload{}, err
		}
		if job.State != exportComplete {
			return ExportDownload{}, ErrStaffConflict
		}
		details := map[string]any{"export_id": job.ID.String(), "row_count": job.RowCount}
		if err = auditStaff(ctx, q, actor.ID, "data.export_downloaded", job.Dataset, details, now); err != nil {
			return ExportDownload{}, err
		}
		return ExportDownload{Dataset: job.Dataset, db: s.DB, job: job}, nil
	})
}

// WriteTo streams the file as CSV, one stored chunk at a time, so no
// transaction or snapshot outlives a chunk read. Its end, complete or
// interrupted, is audited.
func (d ExportDownload) WriteTo(ctx context.Context, destination io.Writer) (resultErr error) {
	completed := false
	defer func() {
		auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		now := time.Now().UTC()
		details := map[string]any{"export_id": d.job.ID.String(), "completed": completed, "finished_at": now}
		err := auditStaff(auditCtx, queries.New(d.db), d.job.ActorID, "data.export_download_finished", d.Dataset, details, now)
		if resultErr == nil && err != nil {
			resultErr = err
		}
	}()
	q := queries.New(d.db)
	for seq := range d.job.ChunkCount {
		stored, err := q.ExportChunk(ctx, queries.ExportChunkParams{JobID: d.job.ID, Seq: seq})
		if err != nil {
			return err
		}
		reader, err := gzip.NewReader(bytes.NewReader(stored))
		if err != nil {
			return err
		}
		if _, err = io.Copy(destination, reader); err != nil {
			return err
		}
	}
	completed = true
	return nil
}
