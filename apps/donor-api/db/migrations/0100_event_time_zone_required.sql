-- Every event insert names its zone (staff input or the ZIP's zone), so a
-- missing one is an error instead of a silent America/New_York.
ALTER TABLE donor_events ALTER COLUMN time_zone DROP DEFAULT;
