ALTER TABLE donors ADD COLUMN timezone_override varchar(64)
  CHECK (timezone_override IS NULL OR timezone_override ~ '^[A-Za-z0-9_+/-]{1,64}$'),
  ADD CONSTRAINT ck_donor_timezone_override CHECK
    (timezone_override IS NULL OR (timezone IS NOT NULL AND timezone=timezone_override));
