-- name: AdmitClientRequest :execrows
-- Admits a request when the client's class allows one now: one row, or none
-- when the client must wait.
INSERT INTO donor_rate_limits AS r (class, client, tat)
VALUES (sqlc.arg(class), sqlc.arg(client), clock_timestamp() + make_interval(secs => sqlc.arg(interval_seconds)::float8))
ON CONFLICT (class, client) DO UPDATE
SET tat = GREATEST(r.tat, clock_timestamp()) + make_interval(secs => sqlc.arg(interval_seconds)::float8)
WHERE r.tat - clock_timestamp() <= make_interval(secs => sqlc.arg(tolerance_seconds)::float8);

-- name: ClientRequestWait :one
SELECT GREATEST(0, EXTRACT(EPOCH FROM tat - clock_timestamp())::float8 - sqlc.arg(tolerance_seconds)::float8)::float8 AS wait_seconds
FROM donor_rate_limits WHERE class = sqlc.arg(class) AND client = sqlc.arg(client);
