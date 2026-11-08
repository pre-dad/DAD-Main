-- Saving recognition rules bumps this epoch instead of rewriting every
-- projection. A projection is current when its rules_version is at least the
-- code's projectionRules plus the epoch, so the projections worker recomputes
-- older ones in bounded batches at its own pace.
CREATE TABLE donor_recognition_epoch (
 singleton BOOLEAN PRIMARY KEY DEFAULT true CHECK(singleton),
 epoch INTEGER NOT NULL DEFAULT 0 CHECK(epoch >= 0)
);
INSERT INTO donor_recognition_epoch(singleton, epoch) VALUES (true, 0);
