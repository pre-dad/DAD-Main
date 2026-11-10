-- A new SMS code resets its own attempt counter, so a resend every minute would
-- otherwise allow 300 guesses an hour. window_attempts counts wrong guesses for
-- a phone across resends within a rolling day that starts at window_started_at.
ALTER TABLE donor_phone_codes
	ADD COLUMN window_started_at TIMESTAMP WITH TIME ZONE DEFAULT now() NOT NULL,
	ADD COLUMN window_attempts INTEGER DEFAULT 0 NOT NULL,
	ADD CONSTRAINT ck_donor_phone_codes_window_attempts CHECK (window_attempts BETWEEN 0 AND 1000);
