-- name: InsertEvent :exec
INSERT INTO donor_events
  (id, slug, title, type, description, venue, address, city, zip, starts_at, ends_at, capacity, qualifying_perk, created_at, updated_at,
   booking_opens_at, time_zone)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$14,$15,$16);

-- name: UpdateEvent :exec
UPDATE donor_events
SET title=$2, type=$3, description=$4, venue=$5, address=$6, city=$7, zip=$8,
    starts_at=$9, ends_at=$10, capacity=$11, qualifying_perk=$12, updated_at=$13, booking_opens_at=$14, time_zone=$15
WHERE id=$1;

-- name: PublishEvent :exec
UPDATE donor_events
SET publish_state='PUBLISHED', published_at=$2, updated_at=$2
WHERE id=$1;

-- name: EventByID :one
SELECT * FROM donor_events WHERE id=$1;

-- name: StaffEventsPage :many
SELECT * FROM donor_events
ORDER BY starts_at DESC, id
LIMIT 51 OFFSET sqlc.arg(page_offset)::bigint;

-- name: EventReservationCounts :many
-- Seats taken and waitlist length per event, for the staff list.
SELECT event_id,
  count(*) FILTER (WHERE status='RESERVED')::bigint AS reserved,
  count(*) FILTER (WHERE status='WAITLISTED')::bigint AS waitlisted,
  count(*) FILTER (WHERE checked_in_at IS NOT NULL AND status='RESERVED')::bigint AS checked_in
FROM donor_event_reservations
WHERE event_id = ANY(sqlc.arg(event_ids)::uuid[])
GROUP BY event_id;

-- name: EventRoster :many
-- §8 RSVPs: everyone holding a seat, then the waitlist in promotion order.
SELECT r.donor_id, r.status, r.reserved_at, r.checked_in_at, d.email, d.public_name
FROM donor_event_reservations r
JOIN donors d ON d.id = r.donor_id
WHERE r.event_id = $1 AND r.status IN ('RESERVED', 'WAITLISTED')
ORDER BY r.status = 'WAITLISTED', r.reserved_at, r.id;

-- name: CancelEvent :execrows
UPDATE donor_events SET cancelled_at=$2, updated_at=$2 WHERE id=$1 AND cancelled_at IS NULL;

-- name: ReleaseEventReservations :many
-- Cancels every live reservation of a cancelled event, returning who held one.
UPDATE donor_event_reservations SET status='CANCELLED', updated_at=$2
WHERE event_id=$1 AND status IN ('RESERVED', 'WAITLISTED')
RETURNING donor_id;

-- name: UpcomingEventsNearDonor :many
-- E-2: published events in and near the donor's city (same city label OR same
-- three-digit ZIP region), upcoming only, soonest first. The near-city match
-- is empty-safe: a donor with no city/zip matches nothing on that predicate.
-- INVITE_ONLY events are excluded here; they surface only through the 4.4
-- gated path (qualifying badge or admin guest list), never the open list.
SELECT * FROM donor_events
WHERE publish_state='PUBLISHED'
  AND cancelled_at IS NULL
  AND type <> 'INVITE_ONLY'
  AND starts_at > sqlc.arg(now)::timestamptz
  AND (
    (sqlc.arg(city)::text <> '' AND lower(city) = lower(sqlc.arg(city)::text))
    OR (sqlc.arg(zip3)::text <> '' AND left(zip, 3) = sqlc.arg(zip3)::text)
  )
ORDER BY starts_at ASC, id
LIMIT 100;

-- name: LockEvent :one
-- Serializes capacity decisions: a reserve/cancel holds this row lock so two
-- donors cannot each take the last seat.
SELECT * FROM donor_events WHERE id=$1 FOR UPDATE;

-- name: ReservedCount :one
SELECT count(*) FROM donor_event_reservations
WHERE event_id=$1 AND status='RESERVED';

-- name: ReservedCountsForEvents :many
-- RESERVED count per event, for the near-city list's spots-left without N+1.
SELECT event_id, count(*)::bigint AS reserved
FROM donor_event_reservations
WHERE event_id = ANY(sqlc.arg(event_ids)::uuid[]) AND status='RESERVED'
GROUP BY event_id;

-- name: DonorReservationsForEvents :many
-- The signed-in donor's active reservation status per event, for the list.
SELECT event_id, status, checked_in_at FROM donor_event_reservations
WHERE donor_id=$1 AND event_id = ANY(sqlc.arg(event_ids)::uuid[])
  AND status IN ('RESERVED', 'WAITLISTED');

-- name: DonorUpcomingReservations :many
SELECT r.event_id, r.status FROM donor_event_reservations r
JOIN donor_events e ON e.id = r.event_id
WHERE r.donor_id = $1 AND r.status IN ('RESERVED', 'WAITLISTED') AND e.starts_at > sqlc.arg(as_of)::timestamptz
ORDER BY r.event_id;

-- name: ReservationFor :one
SELECT * FROM donor_event_reservations WHERE event_id=$1 AND donor_id=$2;

-- name: UpsertReservation :exec
-- Reuses the (event,donor) row when a donor re-reserves after cancelling;
-- reserved_at is only set on the row's first non-cancelled entry (passed in).
INSERT INTO donor_event_reservations (id, event_id, donor_id, status, reserved_at, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$6)
ON CONFLICT (event_id, donor_id) DO UPDATE
SET status=EXCLUDED.status, reserved_at=EXCLUDED.reserved_at, updated_at=EXCLUDED.updated_at;

-- name: SetReservationStatus :exec
UPDATE donor_event_reservations SET status=$3, updated_at=$4 WHERE event_id=$1 AND donor_id=$2;

-- name: WaitlistHead :one
-- The earliest-queued waitlisted donor for an event, the one promoted when a
-- seat frees (E-4 FIFO).
SELECT * FROM donor_event_reservations
WHERE event_id=$1 AND status='WAITLISTED'
ORDER BY reserved_at ASC, id ASC
LIMIT 1;

-- name: PromoteReservation :exec
UPDATE donor_event_reservations SET status='RESERVED', updated_at=$2 WHERE id=$1;

-- name: QueueEventPush :exec
-- Queues one push notification for a donor about an event (e.g. waitlist
-- promotion). dedup_key makes it idempotent; the worker gates on the donor's
-- category preference and token at send time, so this only enqueues.
INSERT INTO donor_push_notifications (id, donor_id, category, dedup_key, context, available_at, expires_at)
VALUES ($1,$2,$3,$4,$5,$6,$7)
ON CONFLICT (dedup_key) DO NOTHING;

-- name: AddEventGuest :exec
INSERT INTO donor_event_guests (id, event_id, donor_id, created_at)
VALUES ($1,$2,$3,$4)
ON CONFLICT (event_id, donor_id) DO NOTHING;

-- name: RemoveEventGuest :exec
DELETE FROM donor_event_guests WHERE event_id=$1 AND donor_id=$2;

-- name: IsEventGuest :one
SELECT EXISTS(SELECT 1 FROM donor_event_guests WHERE event_id=$1 AND donor_id=$2);

-- name: DonorGuestEvents :many
SELECT event_id FROM donor_event_guests
WHERE donor_id=$1 AND event_id = ANY(sqlc.arg(event_ids)::uuid[]);

-- name: DonorEarnedBadgeCodes :many
-- The donor's earned, non-revoked badge codes — the perk side of the E-5 gate.
SELECT code FROM donor_badges WHERE donor_id=$1 AND revoked_at IS NULL;

-- name: UpcomingInviteOnlyEvents :many
-- Published, upcoming INVITE_ONLY events, soonest first. The caller applies the
-- E-5 gate (perk held OR on the guest list) in memory over this candidate set.
SELECT * FROM donor_events
WHERE publish_state='PUBLISHED'
  AND cancelled_at IS NULL
  AND type='INVITE_ONLY'
  AND starts_at > sqlc.arg(now)::timestamptz
ORDER BY starts_at ASC, id
LIMIT 100;

-- name: CheckInReservation :exec
-- Stamps the donor's reservation as checked in (E-8). Only sets it when a
-- reservation exists and is not already checked in; cancelled reservations do
-- not qualify.
UPDATE donor_event_reservations
SET checked_in_at=$3, updated_at=$3
WHERE event_id=$1 AND donor_id=$2 AND status<>'CANCELLED' AND checked_in_at IS NULL;

-- name: DonorVolunteerStats :one
-- "Events volunteered" and volunteer hours (EM-8): the count of a donor's
-- checked-in events and the sum of their durations in minutes. A donor with no
-- check-ins gets zeroes.
SELECT
  count(*)::bigint AS events,
  coalesce(sum(EXTRACT(EPOCH FROM (e.ends_at - e.starts_at)) / 60), 0)::bigint AS minutes
FROM donor_event_reservations r
JOIN donor_events e ON e.id = r.event_id
WHERE r.donor_id=$1 AND r.checked_in_at IS NOT NULL;

-- name: QueueEventReminders :execrows
-- E-7: queue an EVENT_REMINDER push for every reserved (non-cancelled) donor of
-- a published event starting within the given lead window, in one set-based
-- pass. dedup_key is per (event, donor, window label) so each of the 24h and 2h
-- windows fires at most once; expires_at is the event start, so a reminder the
-- worker could not deliver before the event lapses rather than arriving late.
-- window_label distinguishes the two sweeps ('24h' / '2h').
INSERT INTO donor_push_notifications (id, donor_id, category, dedup_key, context, available_at, expires_at)
SELECT gen_random_uuid(), r.donor_id, 'EVENT_REMINDER',
       'push:event-reminder:' || e.id::text || ':' || r.donor_id::text || ':' || sqlc.arg(window_label)::text,
       json_build_object('event_id', e.id::text, 'window', sqlc.arg(window_label)::text),
       sqlc.arg(as_of)::timestamptz, e.starts_at
FROM donor_events e
JOIN donor_event_reservations r ON r.event_id = e.id
WHERE e.publish_state = 'PUBLISHED'
  AND r.status = 'RESERVED'
  AND e.starts_at > sqlc.arg(as_of)::timestamptz
  AND e.starts_at <= sqlc.arg(until)::timestamptz
ON CONFLICT (dedup_key) DO NOTHING;


-- name: QueueEventAlertJob :execrows
INSERT INTO donor_event_alert_jobs(event_id,updated_at)
SELECT id,sqlc.arg(as_of)::timestamptz FROM donor_events
WHERE id=sqlc.arg(event_id) AND publish_state='PUBLISHED' AND cancelled_at IS NULL
  AND type<>'INVITE_ONLY' AND starts_at>sqlc.arg(as_of)::timestamptz
ON CONFLICT(event_id) DO NOTHING;

-- name: NextEventAlertJob :one
SELECT * FROM donor_event_alert_jobs WHERE completed_at IS NULL
ORDER BY updated_at,event_id LIMIT 1 FOR UPDATE SKIP LOCKED;

-- name: QueueEventAlertBatch :one
-- Each audience (the event's city, its ZIP prefix) is read in donor order from
-- its own index, so a batch costs its size however large the audience is; a
-- donor in both is queued once.
WITH target AS (
  SELECT e.id,e.city,e.zip,e.starts_at FROM donor_events e
  WHERE e.id=sqlc.arg(event_id) AND e.publish_state='PUBLISHED' AND e.cancelled_at IS NULL
    AND e.type<>'INVITE_ONLY' AND e.starts_at>sqlc.arg(as_of)::timestamptz
), recipients AS MATERIALIZED (
  SELECT id,event_id,starts_at FROM (
    (SELECT d.id,e.id AS event_id,e.starts_at
     FROM target e
     JOIN donors d ON e.city<>'' AND lower(d.city)=lower(e.city) AND NOT d.disabled
       AND (sqlc.narg(after_donor)::uuid IS NULL OR d.id>sqlc.narg(after_donor)::uuid)
     JOIN donor_preferences p ON p.donor_id=d.id AND p.volunteer AND p.push_event_nearby
     ORDER BY d.id LIMIT sqlc.arg(batch_size)::int)
    UNION
    (SELECT d.id,e.id AS event_id,e.starts_at
     FROM target e
     JOIN donors d ON e.zip<>'' AND left(d.zip,3)=left(e.zip,3) AND NOT d.disabled
       AND (sqlc.narg(after_donor)::uuid IS NULL OR d.id>sqlc.narg(after_donor)::uuid)
     JOIN donor_preferences p ON p.donor_id=d.id AND p.volunteer AND p.push_event_nearby
     ORDER BY d.id LIMIT sqlc.arg(batch_size)::int)
  ) audience
  ORDER BY id LIMIT sqlc.arg(batch_size)::int
), queued AS (
  INSERT INTO donor_push_notifications(id,donor_id,category,dedup_key,context,available_at,expires_at)
  SELECT gen_random_uuid(),id,'EVENT_NEARBY','push:event-nearby:'||event_id::text||':'||id::text,
    json_build_object('event_id',event_id::text),sqlc.arg(as_of)::timestamptz,starts_at
  FROM recipients ON CONFLICT(dedup_key) DO NOTHING RETURNING id
)
SELECT (SELECT id FROM recipients ORDER BY id DESC LIMIT 1)::uuid AS last_donor,
  (SELECT count(*) FROM recipients)::bigint AS considered,
  (SELECT count(*) FROM queued)::bigint AS queued;

-- name: SaveEventAlertProgress :exec
UPDATE donor_event_alert_jobs SET last_donor_id=coalesce($2,last_donor_id),completed_at=$3,updated_at=$4
WHERE event_id=$1;
