-- name: DonorRanking :one
-- A donor's own ranking row (K-3: only ever their own position).
SELECT r.national_percentile, r.city, r.city_rank,
  coalesce((SELECT population FROM donor_ranking_populations WHERE scope='NATIONAL' AND city=''), 0)::int AS national_population,
  coalesce((SELECT population FROM donor_ranking_populations WHERE scope='CITY' AND city=r.city), 0)::int AS city_population,
  r.computed_at
FROM donor_rankings r
JOIN donors d ON d.id=r.donor_id
JOIN donor_metric_projections p ON p.donor_id=r.donor_id
LEFT JOIN donor_preferences pref ON pref.donor_id=r.donor_id
WHERE r.donor_id=$1 AND NOT d.disabled AND NOT coalesce(pref.rankings_opted_out,false)
  AND p.current_streak>0 AND p.streak_since IS NOT NULL AND p.next_refresh_at>clock_timestamp();

-- name: ClaimProjectionSchedule :execrows
INSERT INTO donor_projection_schedules(id,next_run_at) VALUES($1,$2)
ON CONFLICT(id) DO UPDATE SET next_run_at=EXCLUDED.next_run_at
WHERE donor_projection_schedules.next_run_at <= sqlc.arg(as_of)::timestamptz;

-- name: SaveProjectionSchedule :exec
INSERT INTO donor_projection_schedules(id,next_run_at) VALUES($1,$2)
ON CONFLICT(id) DO UPDATE SET next_run_at=EXCLUDED.next_run_at;
