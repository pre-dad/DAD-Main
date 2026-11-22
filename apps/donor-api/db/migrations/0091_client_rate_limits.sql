-- Per-client request limits shared by every API replica. tat is the client's
-- theoretical arrival time in the generic cell rate algorithm; a row whose tat
-- has passed admits the same as no row, so retention deletes it. The table is
-- unlogged and unindexed beyond its key: losing it in a crash only resets the
-- limits, and every admission rewrites tat, which an index would make costly.
CREATE UNLOGGED TABLE donor_rate_limits (
    class VARCHAR(16) NOT NULL,
    client VARCHAR(64) NOT NULL,
    tat TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (class, client)
) WITH (fillfactor = 70);
