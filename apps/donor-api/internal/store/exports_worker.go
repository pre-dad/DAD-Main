package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// exportBatchRows is how many rows one step reads. A step is a single
	// short transaction, so no export holds a snapshot or a connection for
	// longer than that.
	exportBatchRows = 5000
	// exportAttempts is how many failed steps fail a job.
	exportAttempts   = 5
	exportPurgeBatch = 20
	exportPurgeEvery = 10 * time.Minute
)

// ExportWorker builds staff CSV exports in bounded keyset steps. Each step
// reads one batch past the job's cursor, stores it as a gzip member, and moves
// the cursor in one transaction: a step that dies leaves nothing behind and the
// next resumes at the same row, so a restart repeats no row and loses none.
//
// A file is consistent as of its request only where the dataset has an
// insert-time timestamp (exportAsOf). Elsewhere each row is read as it stands
// when the cursor reaches it.
type ExportWorker struct {
	DB *pgxpool.Pool
	// BatchRows and MaxBytes override exportBatchRows and maxExportBytes; tests
	// set them small.
	BatchRows int
	MaxBytes  int64

	nextPurge time.Time
}

// RunNext does one unit of work: a step of the oldest unfinished export, or
// otherwise a batch of expired-export cleanup once an interval. A failing step
// is recorded against its job and logged without error text, so it never holds
// up the work sharing the worker.
func (w *ExportWorker) RunNext(ctx context.Context, now time.Time) bool {
	worked, err := w.ProcessNext(ctx, now)
	if err == nil && !worked {
		worked, err = w.PurgeNext(ctx, now)
	}
	if err != nil && ctx.Err() == nil {
		slog.Error("export step failed", "cause", fmt.Sprintf("%T", err))
	}
	return worked
}

// ProcessNext advances the oldest unfinished export by one batch.
func (w *ExportWorker) ProcessNext(ctx context.Context, now time.Time) (bool, error) {
	var claimed pgtype.UUID
	worked, err := transact(ctx, w.DB, pgx.TxOptions{}, func(tx pgx.Tx) (bool, error) {
		q := queries.New(tx)
		job, err := q.ClaimExportJob(ctx, timestamp(now))
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		claimed = job.ID
		return true, w.step(ctx, tx, job, now)
	})
	if err != nil && claimed.Valid && ctx.Err() == nil {
		err = errors.Join(err, w.recordFailure(ctx, claimed, now))
	}
	return worked, err
}

// recordFailure counts a failed step against the job, outside the step's
// rolled-back transaction, and ends the job once it keeps failing.
func (w *ExportWorker) recordFailure(ctx context.Context, id pgtype.UUID, now time.Time) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return pgx.BeginFunc(ctx, w.DB, func(tx pgx.Tx) error {
		q := queries.New(tx)
		job, err := q.RecordExportJobFailure(
			ctx,
			queries.RecordExportJobFailureParams{ID: id, LimitAttempts: exportAttempts, AsOf: timestamp(now)},
		)
		if err != nil || job.State != exportFailed {
			return err
		}
		if err = q.DeleteExportChunks(ctx, id); err != nil {
			return err
		}
		return finishAudit(ctx, q, job, false, exportError, now)
	})
}

func (w *ExportWorker) step(ctx context.Context, tx pgx.Tx, job queries.DonorExportJob, now time.Time) error {
	q := queries.New(tx)
	dataset, known := exportDatasets[job.Dataset]
	staff, err := q.ExportJobStaff(ctx, job.ActorID)
	if err != nil {
		return err
	}
	if !known || staff.Disabled || !permitted(staff.Role, job.Dataset) {
		return failExport(ctx, q, job, exportForbidden, now)
	}
	limit := exportBatchRows
	if w.BatchRows > 0 {
		limit = w.BatchRows
	}
	var chunk bytes.Buffer
	compressor := gzip.NewWriter(&chunk)
	writer := csv.NewWriter(compressor)
	writer.UseCRLF = true
	fields := strings.Fields(dataset.fields)
	if job.ChunkCount == 0 {
		if err = writer.Write(fields); err != nil {
			return err
		}
	}
	cursor, rows, err := readBatch(ctx, tx, writer, job, dataset, fields, limit)
	if err != nil {
		return err
	}
	writer.Flush()
	if err = errors.Join(writer.Error(), compressor.Close()); err != nil {
		return err
	}
	size := int64(chunk.Len())
	ceiling := int64(maxExportBytes)
	if w.MaxBytes > 0 {
		ceiling = w.MaxBytes
	}
	if job.StoredBytes+size > ceiling {
		return failExport(ctx, q, job, exportTooLarge, now)
	}
	if err = q.InsertExportChunk(ctx, queries.InsertExportChunkParams{JobID: job.ID, Seq: job.ChunkCount, Data: chunk.Bytes()}); err != nil {
		return err
	}
	if rows == limit {
		return q.AdvanceExportJob(ctx, queries.AdvanceExportJobParams{Cursor: cursor, Rows: int64(rows), Bytes: size, ID: job.ID})
	}
	err = q.CompleteExportJob(ctx, queries.CompleteExportJobParams{
		Cursor: cursor, Rows: int64(rows), Bytes: size, CompletedAt: timestamp(now), ExpiresAt: timestamp(now.Add(exportRetention)), ID: job.ID,
	})
	if err != nil {
		return err
	}
	job.RowCount += int64(rows)
	return finishAudit(ctx, q, job, true, "", now)
}

// readBatch writes the next rows after the job's cursor and returns the cursor
// past them. SQL identifiers come only from the fixed dataset allowlist.
func readBatch(
	ctx context.Context,
	tx pgx.Tx,
	writer *csv.Writer,
	job queries.DonorExportJob,
	dataset exportDataset,
	fields []string,
	limit int,
) (pgtype.UUID, int, error) {
	columns := make([]string, 0, len(fields)+1)
	columns = append(columns, "id")
	for _, field := range fields {
		columns = append(columns, pgx.Identifier{field}.Sanitize())
	}
	query := "SELECT " + strings.Join(
		columns,
		",",
	) + " FROM " + pgx.Identifier{
		dataset.table,
	}.Sanitize() + " WHERE ($1::uuid IS NULL OR id>$1)"
	if column := exportAsOf[job.Dataset]; column != "" {
		query += " AND " + pgx.Identifier{column}.Sanitize() + "<=$2"
	}
	if job.Dataset == "referrals" {
		query += " AND referrer_id IS NOT NULL"
	}
	query += " ORDER BY id LIMIT " + strconv.Itoa(limit)
	arguments := []any{job.Cursor}
	if strings.Contains(query, "<=$2") {
		arguments = append(arguments, job.RequestedAt)
	}
	rows, err := tx.Query(ctx, query, arguments...)
	if err != nil {
		return pgtype.UUID{}, 0, err
	}
	defer rows.Close()
	cursor, count := job.Cursor, 0
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return cursor, count, err
		}
		cursor = pgtype.UUID{Bytes: values[0].([16]byte), Valid: true}
		cells := make([]string, len(fields))
		for i, value := range values[1:] {
			if cells[i], err = safeCSVCell(value); err != nil {
				return cursor, count, err
			}
		}
		if err = writer.Write(cells); err != nil {
			return cursor, count, err
		}
		count++
	}
	return cursor, count, rows.Err()
}

// failExport ends a job that can never finish, discarding what it stored.
func failExport(ctx context.Context, q *queries.Queries, job queries.DonorExportJob, reason string, now time.Time) error {
	if err := q.DeleteExportChunks(ctx, job.ID); err != nil {
		return err
	}
	err := q.FailExportJob(ctx, queries.FailExportJobParams{
		Failure: text(reason), CompletedAt: timestamp(now), ExpiresAt: timestamp(now.Add(exportRetention)), ID: job.ID,
	})
	if err != nil {
		return err
	}
	return finishAudit(ctx, q, job, false, reason, now)
}

// finishAudit records how an export ended, complete or not.
func finishAudit(ctx context.Context, q *queries.Queries, job queries.DonorExportJob, completed bool, failure string, now time.Time) error {
	details := map[string]any{"export_id": job.ID.String(), "completed": completed, "row_count": job.RowCount, "finished_at": now}
	if failure != "" {
		details["failure"] = failure
	}
	return auditStaff(ctx, q, job.ActorID, "data.export_finished", job.Dataset, details, now)
}

// PurgeNext deletes one batch of exports past their retention, with their
// stored chunks, at most once an interval until a batch comes up short.
func (w *ExportWorker) PurgeNext(ctx context.Context, now time.Time) (bool, error) {
	if now.Before(w.nextPurge) {
		return false, nil
	}
	purged, err := queries.New(w.DB).
		PurgeExpiredExports(ctx, queries.PurgeExpiredExportsParams{ExpiredBefore: timestamp(now), Batch: exportPurgeBatch})
	if err != nil {
		w.nextPurge = now.Add(exportPurgeEvery)
		return false, err
	}
	if purged < exportPurgeBatch {
		w.nextPurge = now.Add(exportPurgeEvery)
	}
	return purged > 0, nil
}
