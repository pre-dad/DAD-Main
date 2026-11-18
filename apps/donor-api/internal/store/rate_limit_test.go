package store

import (
	"context"
	"testing"
	"time"
)

func TestClientRateLimitIsSharedByEveryReplica(t *testing.T) {
	ctx := context.Background()
	client := "198.51.100." + testID(t).String()[:8]
	t.Cleanup(func() { cleanExec(t, "DELETE FROM donor_rate_limits WHERE client=$1", client) })
	replicas := []ClientRateLimit{{DB: database(t)}, {DB: database(t)}}
	for i := range 6 {
		if wait, err := replicas[i%2].Admit(ctx, "account", client, 6); err != nil || wait != 0 {
			t.Fatalf("request %d of the burst: wait %v, %v", i+1, wait, err)
		}
	}
	wait, err := replicas[0].Admit(ctx, "account", client, 6)
	if err != nil || wait <= 9*time.Second || wait > 10*time.Second {
		t.Fatalf("over the shared burst: wait %v, %v; want about the 10s one request takes to refill", wait, err)
	}
	if wait, err := replicas[1].Admit(ctx, "visitor", client, 6); err != nil || wait != 0 {
		t.Fatalf("another class: wait %v, %v", wait, err)
	}

	// A refused request spends nothing, so the client is admitted again once
	// one request has refilled.
	mustExec(t, replicas[0].DB, "UPDATE donor_rate_limits SET tat=tat-interval '10 seconds' WHERE client=$1 AND class='account'", client)
	if wait, err := replicas[1].Admit(ctx, "account", client, 6); err != nil || wait != 0 {
		t.Fatalf("after one refill: wait %v, %v", wait, err)
	}

	mustExec(t, replicas[0].DB, "UPDATE donor_rate_limits SET tat=now()-interval '1 second' WHERE client=$1", client)
	if _, err := (&TouchRetention{DB: replicas[0].DB}).PurgeNext(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := replicas[0].DB.QueryRow(ctx, "SELECT count(*) FROM donor_rate_limits WHERE client=$1", client).Scan(&left); err != nil || left != 0 {
		t.Fatalf("spent limits after retention: %d, %v", left, err)
	}
}
