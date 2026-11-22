-- A phone number identifies one donor: phone sign-in looks the donor up by number.
-- Where several donors already hold the same number, the most recently claimed
-- account keeps it and the others are detached; they can add a number again.
UPDATE donors d SET phone = NULL
FROM (
 SELECT id, row_number() OVER (PARTITION BY phone ORDER BY claimed_at DESC NULLS LAST, created_at DESC, id) AS rank
 FROM donors WHERE phone IS NOT NULL
) ranked
WHERE ranked.id = d.id AND ranked.rank > 1;

DROP INDEX ix_donors_phone;
CREATE UNIQUE INDEX ux_donors_phone ON donors (phone) WHERE phone IS NOT NULL;
