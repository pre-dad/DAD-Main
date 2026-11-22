package httpapi_test

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dad/donor-api/internal/httpapi"
	"dad/donor-api/internal/store"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// deleteDonorRows removes a synthetic donor and the rows the sign-in journey
// leaves behind for it.
func deleteDonorRows(t *testing.T, pool *pgxpool.Pool, donor pgtype.UUID) {
	t.Helper()
	for _, table := range []string{
		"donor_preference_events", "donor_preferences", "donor_metric_projections", "donor_session_events",
		"donor_sessions", "donor_login_links", "donor_device_marks",
	} {
		if _, err := pool.Exec(context.Background(), "DELETE FROM "+table+" WHERE donor_id=$1", donor); err != nil {
			t.Error(err)
		}
	}
	if _, err := pool.Exec(context.Background(), "DELETE FROM donors WHERE id=$1", donor); err != nil {
		t.Error(err)
	}
}

// This opt-in server exists only in the test binary and uses the lifecycle
// test's synthetic database and least-privilege API pool.
func serveNativeClients(t *testing.T, directory string, owner, api *pgxpool.Pool) {
	t.Helper()
	info, err := os.Stat(directory)
	if err != nil || !filepath.IsAbs(directory) || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("DONOR_NATIVE_ACCEPTANCE_DIR must be an absolute, private test directory")
	}
	server := httptest.NewTLSServer(httpapi.NewServer("", httpapi.Services{
		Origin: "https://donor.example.org", Account: store.AccountService{DB: api},
		Donor: store.DonorService{DB: api, Origin: "https://donor.example.org"},
	}).Handler)
	defer server.Close()
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	for _, platform := range []string{"ios", "android"} {
		fixture := map[string]string{"origin": server.URL, "certificate_pem": string(certificate)}
		for _, field := range []string{"claim_token", "logout_claim_token"} {
			donor, err := store.NewID()
			if err != nil {
				t.Fatal(err)
			}
			email := platform + "-native-" + donor.String() + "@example.org"
			if _, err := owner.Exec(context.Background(), "INSERT INTO donors(id,email) VALUES($1,$2)", donor, email); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { deleteDonorRows(t, owner, donor) })
			if field == "claim_token" {
				fixture["email"] = email
			}
			id, err := store.NewID()
			if err != nil {
				t.Fatal(err)
			}
			proof := strings.Repeat("n", 11) + id.String()
			_, err = owner.Exec(context.Background(), `INSERT INTO donor_login_links(id,donor_id,token_hash,expires_at)
				VALUES($1,$2,$3,$4)`, id, donor, store.HashToken(proof), time.Now().Add(15*time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			fixture[field] = proof
		}
		body, err := json.Marshal(fixture)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, platform+".json"), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "ready"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(8 * time.Minute)
	defer timer.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-timer.C:
			t.Fatal("native clients did not complete within eight minutes")
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(directory, "done")); err == nil {
				return
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		}
	}
}
