-- name: PurgeDonorSessions :execrows
-- One bounded batch of sessions whose last credential lapsed before the cutoff,
-- oldest first.
DELETE FROM donor_sessions WHERE id=ANY(ARRAY(
 SELECT id FROM donor_sessions WHERE coalesce(refresh_expires_at,expires_at)<sqlc.arg(lapsed_before)::timestamptz
 ORDER BY coalesce(refresh_expires_at,expires_at),id LIMIT sqlc.arg(batch)
));

-- name: PurgeLoginLinks :execrows
DELETE FROM donor_login_links WHERE id=ANY(ARRAY(
 SELECT id FROM donor_login_links WHERE expires_at<sqlc.arg(expired_before)::timestamptz
 ORDER BY expires_at,id LIMIT sqlc.arg(batch)
));

-- name: PurgeLoginEmails :execrows
DELETE FROM donor_login_emails WHERE id=ANY(ARRAY(
 SELECT id FROM donor_login_emails WHERE completed_at<sqlc.arg(completed_before)::timestamptz
 ORDER BY completed_at,id LIMIT sqlc.arg(batch)
));

-- name: PurgePhoneCodes :execrows
DELETE FROM donor_phone_codes WHERE phone=ANY(ARRAY(
 SELECT phone FROM donor_phone_codes WHERE expires_at<sqlc.arg(expired_before)::timestamptz
 ORDER BY expires_at,phone LIMIT sqlc.arg(batch)
));
