-- name: ClaimProjectionSchedule :execrows
INSERT INTO donor_projection_schedules(id,next_run_at) VALUES($1,$2)
ON CONFLICT(id) DO UPDATE SET next_run_at=EXCLUDED.next_run_at
WHERE donor_projection_schedules.next_run_at <= sqlc.arg(as_of)::timestamptz;

-- name: SaveProjectionSchedule :exec
INSERT INTO donor_projection_schedules(id,next_run_at) VALUES($1,$2)
ON CONFLICT(id) DO UPDATE SET next_run_at=EXCLUDED.next_run_at;
