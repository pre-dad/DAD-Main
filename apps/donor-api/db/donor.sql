-- name: SavePreference :exec
INSERT INTO donor_preferences (id,donor_id,version,referral_emails,marketing_emails,
 push_referral_converted,push_streak_at_risk,push_streak_milestone,rankings_opted_out,network_visible,
 volunteer,volunteer_interests,push_streak_lapsed,push_badge_earned,
 push_event_nearby,push_event_reminder,push_event_promoted,push_gift_processed)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18) ON CONFLICT(donor_id) DO UPDATE
SET version=EXCLUDED.version,referral_emails=EXCLUDED.referral_emails,marketing_emails=EXCLUDED.marketing_emails,
 push_referral_converted=EXCLUDED.push_referral_converted,push_streak_at_risk=EXCLUDED.push_streak_at_risk,
 push_streak_milestone=EXCLUDED.push_streak_milestone,rankings_opted_out=EXCLUDED.rankings_opted_out,
 network_visible=EXCLUDED.network_visible,volunteer=EXCLUDED.volunteer,volunteer_interests=EXCLUDED.volunteer_interests,
 push_streak_lapsed=EXCLUDED.push_streak_lapsed,push_badge_earned=EXCLUDED.push_badge_earned,
 push_event_nearby=EXCLUDED.push_event_nearby,push_event_reminder=EXCLUDED.push_event_reminder,
 push_event_promoted=EXCLUDED.push_event_promoted,push_gift_processed=EXCLUDED.push_gift_processed;

-- name: RecordPreferenceConsent :exec
INSERT INTO donor_preference_events(id,donor_id,version,referral_emails,marketing_emails,
 push_referral_converted,push_streak_at_risk,push_streak_milestone,rankings_opted_out,network_visible,
 volunteer,volunteer_interests,consent_version,push_streak_lapsed,push_badge_earned,
 push_event_nearby,push_event_reminder,push_event_promoted,push_gift_processed,source)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,'account');

-- name: SaveSharing :one
UPDATE donors SET public_name=$2,referral_code=$3 WHERE id=$1 RETURNING *;

-- name: ReferralTouchByToken :one
SELECT * FROM donor_referral_touches WHERE token_hash=$1 FOR UPDATE;

-- name: ReferrerByCode :one
SELECT * FROM donors WHERE referral_code=$1 AND claimed_at IS NOT NULL AND public_name IS NOT NULL AND disabled=false;

-- name: UpsertReferralTouch :exec
-- Last-touch attribution: a visitor's most recent valid code wins (PRD R-4),
-- so a new code overwrites the referrer, kind and window recorded for the
-- token rather than preserving the earlier touch.
INSERT INTO donor_referral_touches (id,token_hash,referrer_id,kind,touched_at,expires_at) VALUES($1,$2,$3,$4,$5,$6)
ON CONFLICT (token_hash) DO UPDATE SET referrer_id=EXCLUDED.referrer_id,kind=EXCLUDED.kind,touched_at=EXCLUDED.touched_at,expires_at=EXCLUDED.expires_at;

-- name: CapturedReferrerName :one
SELECT d.public_name FROM donors d JOIN donor_referral_touches t ON t.referrer_id=d.id
WHERE t.token_hash=$1 AND d.disabled=false;

-- name: DonorGiftsPage :many
SELECT g.id,g.amount_minor,g.currency,g.frequency,g.paid_at,r.refunded_minor,
(g.amount_minor-r.refunded_minor)::bigint AS net_minor
FROM donor_gifts g CROSS JOIN LATERAL (
 SELECT coalesce(sum(amount_minor),0)::bigint AS refunded_minor FROM donor_refunds
 WHERE gift_id=g.id AND status='SUCCEEDED'
) r
WHERE g.donor_id=$1
ORDER BY g.paid_at DESC,g.id DESC LIMIT 51 OFFSET sqlc.arg(page_offset)::bigint;

-- name: PurgeReferralTouches :many
-- A checkout copies its touch's referrer, kind and expiry exactly, and its gift
-- keeps the referrer only if paid before that expiry, so a closed touch with no
-- such gift never converted. Rows are visited in key order after the cursor so
-- that touches kept for good are passed over once per pass.
WITH deleted AS (
 DELETE FROM donor_referral_touches WHERE id=ANY(ARRAY(
  SELECT t.id FROM donor_referral_touches t
  WHERE (t.touched_at,t.id)>(sqlc.arg(after_touched_at)::timestamptz,sqlc.arg(after_id)::uuid)
  AND t.touched_at<sqlc.arg(touched_before)::timestamptz AND t.expires_at<=sqlc.arg(as_of)::timestamptz
  AND NOT EXISTS(SELECT 1 FROM donor_checkouts c JOIN donor_gifts g ON g.checkout_id=c.id
   WHERE c.referrer_id=t.referrer_id AND c.referral_expires_at=t.expires_at AND c.referral_kind=t.kind
   AND g.referrer_id IS NOT NULL)
  ORDER BY t.touched_at,t.id LIMIT sqlc.arg(batch) FOR UPDATE OF t SKIP LOCKED
 )) RETURNING touched_at,id
)
SELECT touched_at,id FROM deleted ORDER BY touched_at,id;


-- name: UpdateDonorProfile :one
-- Replace the donor's self-service identity fields. account_role is deliberately
-- never written here: a role is fixed at row creation and immutable (PRD §3).
UPDATE donors SET zip=$2, city=$3, timezone=$4, phone=$5 WHERE id=$1 RETURNING *;


-- name: UpsertPhoneCode :exec
-- Last request wins: a new code for the same phone overwrites the pending one
-- and resets its attempt counter. The phone's guess window carries over until
-- it is a day old.
INSERT INTO donor_phone_codes (phone, code_hash, attempts, expires_at)
VALUES ($1, $2, 0, $3)
ON CONFLICT (phone) DO UPDATE SET code_hash=EXCLUDED.code_hash, attempts=0, expires_at=EXCLUDED.expires_at, created_at=now(),
 window_attempts=CASE WHEN donor_phone_codes.window_started_at > now() - interval '1 day' THEN donor_phone_codes.window_attempts ELSE 0 END,
 window_started_at=CASE WHEN donor_phone_codes.window_started_at > now() - interval '1 day' THEN donor_phone_codes.window_started_at ELSE now() END;

-- name: PhoneCode :one
SELECT phone, code_hash, attempts, expires_at, created_at, window_started_at, window_attempts
FROM donor_phone_codes WHERE phone=$1 FOR UPDATE;

-- name: RecordPhoneCodeAttempt :exec
UPDATE donor_phone_codes SET attempts=attempts+1, window_attempts=window_attempts+1 WHERE phone=$1;

-- name: DeletePhoneCode :exec
DELETE FROM donor_phone_codes WHERE phone=$1;

-- name: DonorByPhone :one
SELECT * FROM donors WHERE phone=$1 AND disabled=false;

-- name: OIDCIdentity :one
SELECT id, donor_id, provider, subject, created_at, last_used_at FROM donor_oidc_identities
WHERE provider=$1 AND subject=$2;

-- name: InsertOIDCIdentity :exec
INSERT INTO donor_oidc_identities (id, donor_id, provider, subject, last_used_at)
VALUES ($1, $2, $3, $4, $5);

-- name: TouchOIDCIdentity :exec
UPDATE donor_oidc_identities SET last_used_at=$2 WHERE id=$1;


-- name: UpsertCompanyTouch :exec
-- Last touch wins: a new company code for the same visitor token overwrites the
-- code, time and expiry recorded for that token.
INSERT INTO donor_company_touches (id, token_hash, company_code, touched_at, expires_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (token_hash) DO UPDATE
SET company_code=EXCLUDED.company_code, touched_at=EXCLUDED.touched_at, expires_at=EXCLUDED.expires_at;

-- name: CompanyTouchByToken :one
SELECT id, token_hash, company_code, touched_at, expires_at, created_at
FROM donor_company_touches WHERE token_hash=$1;

-- name: BindDonorCompanyCode :exec
-- Record the captured company code on the donor at first gift, only if one is
-- not already set (first bind wins; the touch's last-touch rule already chose
-- which code that is).
UPDATE donors SET company_code=$2 WHERE id=$1 AND company_code IS NULL;

-- name: PurgeCompanyTouches :exec
DELETE FROM donor_company_touches WHERE expires_at<=$1;


-- name: AnonymizeDonor :exec
-- Scrub personal data from the donor row in place, keeping the id the financial
-- ledger references. email is NOT NULL UNIQUE so it becomes a unique non-routable
-- placeholder rather than null; every other personal field is cleared and the
-- account is disabled so it can no longer be used.
UPDATE donors SET
	email = 'deleted-' || id::text || '@deleted.invalid',
	public_name = NULL,
 full_name = NULL,
	referral_code = NULL,
	zip = NULL,
	city = NULL,
	timezone = NULL,
	phone = NULL,
	company_code = NULL,
	disabled = true
WHERE id = $1;

-- name: DeleteDonorOIDCIdentities :exec
DELETE FROM donor_oidc_identities WHERE donor_id=$1;

-- name: DeleteDonorPreferences :exec
DELETE FROM donor_preferences WHERE donor_id=$1;

-- name: ScrubDonorEmailChanges :exec
UPDATE donor_email_changes SET
	old_email = 'deleted-old-' || id::text || '@deleted.invalid',
	new_email = 'deleted-new-' || id::text || '@deleted.invalid',
	encrypted_token = NULL
WHERE donor_id = $1;

-- name: ScrubDonorSessionDevices :exec
UPDATE donor_sessions SET device_name = NULL WHERE donor_id = $1;

-- name: DeleteEmployeeLink :exec
DELETE FROM donor_partner_employees WHERE donor_id = $1;

-- name: RecordDonorDeletion :exec
INSERT INTO donor_deletions (donor_id, deleted_at) VALUES ($1, $2)
ON CONFLICT (donor_id) DO NOTHING;

-- name: DonorPassword :one
SELECT password_hash FROM donor_passwords WHERE donor_id=$1;
-- name: SetDonorPassword :exec
INSERT INTO donor_passwords(donor_id,password_hash,updated_at) VALUES($1,$2,$3)
ON CONFLICT(donor_id) DO UPDATE SET password_hash=EXCLUDED.password_hash,updated_at=EXCLUDED.updated_at;
-- name: DeleteDonorPassword :exec
DELETE FROM donor_passwords WHERE donor_id=$1;
-- name: SaveSignupProfile :exec
UPDATE donors SET full_name=$2,zip=$3,city=$4,timezone=$5 WHERE id=$1;

-- name: MarkPasswordReauthenticated :exec
UPDATE donor_sessions SET password_verified_at=$2 WHERE token_hash=$1;
-- name: PasswordReauthentication :one
SELECT EXISTS(SELECT 1 FROM donor_sessions WHERE token_hash=$1 AND password_verified_at>sqlc.arg(as_of)::timestamptz-interval '15 minutes');
