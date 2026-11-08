-- Retention deletes by age and account erasure deletes by donor; both scanned
-- the whole table, which grows with every client event.
CREATE INDEX ix_analytics_events_received ON analytics_events (received_at);
CREATE INDEX ix_analytics_events_donor ON analytics_events (donor_id) WHERE donor_id IS NOT NULL;
