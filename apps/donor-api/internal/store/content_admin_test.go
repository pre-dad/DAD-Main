package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"dad/donor-api/internal/dbroles"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// cleanSources removes actor's sources after the test.
func cleanSources(t *testing.T, pool *pgxpool.Pool, actor pgtype.UUID) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DELETE FROM donor_content_sources WHERE actor_id=$1", actor); err != nil {
			t.Error(err)
		}
	})
}

func TestSourceUploadsRequireEncryptionKey(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	ctx := context.Background()
	actor, token := staffFixture(t, pool, "SUPER_ADMIN")
	_, support := staffFixture(t, pool, "OP_PARTNERSHIPS")
	cleanSources(t, pool, actor)
	s := ContentService{Staff: &StaffService{DB: api}}
	if err := s.AuthorizeSourceUpload(ctx, support); !errors.Is(err, ErrStaffForbidden) {
		t.Fatal("unauthorized caller learned storage configuration", err)
	}
	if err := s.AuthorizeSourceUpload(ctx, token); !errors.Is(err, ErrSourceStorageUnconfigured) {
		t.Fatal("upload authorized without an encryption key", err)
	}
	if _, err := s.StoreSource(ctx, token, "source.txt", []byte("Private signed source record")); !errors.Is(
		err,
		ErrSourceStorageUnconfigured,
	) {
		t.Fatal("source stored without an encryption key", err)
	}
	var stored int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM donor_content_sources WHERE actor_id=$1", actor).Scan(&stored); err != nil ||
		stored != 0 {
		t.Fatal("unencrypted source persisted", stored, err)
	}
}

func TestSourceStoredBeforeAKeyRotationStaysReadable(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	ctx := context.Background()
	actor, token := staffFixture(t, pool, "SUPER_ADMIN")
	cleanSources(t, pool, actor)
	old := base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	s := ContentService{Staff: &StaffService{DB: api}, EncryptionKeys: keyring(t, old)}
	data := []byte("Private signed source record")
	source, err := s.StoreSource(ctx, token, "source.txt", data)
	if err != nil {
		t.Fatal(err)
	}
	s.EncryptionKeys = keyring(t, base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))+","+old)
	if document, err := s.ReadSource(ctx, token, source.ID); err != nil || !bytes.Equal(document.Data, data) {
		t.Fatal("source unreadable after prepending a key", err)
	}
}

func TestSourceIntegrity(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	ctx := context.Background()
	actor, token := staffFixture(t, pool, "SUPER_ADMIN")
	_, support := staffFixture(t, pool, "OP_PARTNERSHIPS")
	cleanSources(t, pool, actor)
	s := ContentService{Staff: &StaffService{DB: api}, EncryptionKeys: keyring(t, base64.URLEncoding.EncodeToString(make([]byte, 32)))}
	data := []byte("Private signed source record")
	if _, err := s.StoreSource(ctx, support, "source.txt", data); !errors.Is(err, ErrStaffForbidden) {
		t.Fatal("support uploaded source", err)
	}
	source, err := s.StoreSource(ctx, token, "source.txt", data)
	if err != nil {
		t.Fatal(err)
	}
	id := source.ID
	var ciphertext []byte
	if err = pool.QueryRow(ctx, "SELECT encrypted_data FROM donor_content_sources WHERE id=$1", id).Scan(&ciphertext); err != nil ||
		bytes.Contains(ciphertext, data) {
		t.Fatal("source not encrypted", err)
	}
	document, err := s.ReadSource(ctx, token, id)
	if err != nil || !bytes.Equal(document.Data, data) {
		t.Fatal("source roundtrip failed", err)
	}
	if _, err = pool.Exec(ctx, "UPDATE donor_content_sources SET sha256=repeat('0',64) WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReadSource(ctx, token, id); !errors.Is(err, ErrStaffConflict) {
		t.Fatal("corrupt source returned", err)
	}
}

func TestSourceInputsAreBoundedAndEveryUseIsAudited(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	ctx := context.Background()
	actor, token := staffFixture(t, pool, "SUPER_ADMIN")
	_, support := staffFixture(t, pool, "OP_PARTNERSHIPS")
	cleanSources(t, pool, actor)
	s := ContentService{Staff: &StaffService{DB: api}, EncryptionKeys: keyring(t, base64.URLEncoding.EncodeToString(make([]byte, 32)))}
	valid := []byte("Private signed source record")
	for name, c := range map[string]struct {
		file string
		data []byte
	}{
		"empty body":      {"source.txt", nil},
		"oversized body":  {"source.txt", make([]byte, 10*1024*1024+1)},
		"empty name":      {"", valid},
		"control in name": {"line\nbreak.txt", valid},
		"long name":       {strings.Repeat("n", 121), valid},
	} {
		if _, err := s.StoreSource(ctx, token, c.file, c.data); !errors.Is(err, ErrStaffInput) {
			t.Errorf("%s: %v; want ErrStaffInput", name, err)
		}
	}
	atLimit := make([]byte, 10*1024*1024)
	atLimit[0] = 1
	source, err := s.StoreSource(ctx, token, strings.Repeat("é", 120), atLimit)
	if err != nil {
		t.Fatalf("a source at both limits was refused: %v", err)
	}
	var stored int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM donor_content_sources WHERE actor_id=$1", actor).Scan(&stored); err != nil ||
		stored != 1 {
		t.Fatalf("%d sources stored, want only the one accepted: %v", stored, err)
	}
	if _, err = s.ReadSource(ctx, support, source.ID); !errors.Is(err, ErrStaffForbidden) {
		t.Fatal("support read a source", err)
	}
	var missing pgtype.UUID
	if err = missing.Scan("00000000-0000-0000-0000-000000000000"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReadSource(ctx, token, missing); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("unknown source read", err)
	}
	if _, err = s.ReadSource(ctx, token, source.ID); err != nil {
		t.Fatal(err)
	}
	for action, want := range map[string]int{"content.source_uploaded": 1, "content.source_read": 1} {
		var n int
		if err = pool.QueryRow(ctx, "SELECT count(*) FROM donor_staff_audit WHERE actor_id=$1 AND action=$2 AND target=$3", actor, action, source.ID.String()).Scan(&n); err != nil ||
			n != want {
			t.Errorf("%s audited %d times, want %d: %v", action, n, want, err)
		}
	}
}

func TestSourceInventoryListsNamesAndDigestsToPublishersOnly(t *testing.T) {
	pool := database(t)
	api := as(t, dbroles.API)
	ctx := context.Background()
	actor, token := staffFixture(t, pool, "SUPER_ADMIN")
	_, support := staffFixture(t, pool, "OP_PARTNERSHIPS")
	cleanSources(t, pool, actor)
	s := ContentService{Staff: &StaffService{DB: api}, EncryptionKeys: keyring(t, base64.URLEncoding.EncodeToString(make([]byte, 32)))}
	data := []byte("Private signed source record")
	source, err := s.StoreSource(ctx, token, "listed.txt", data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Inventory(ctx, support); !errors.Is(err, ErrStaffForbidden) {
		t.Fatal("support listed sources", err)
	}
	inventory, err := s.Inventory(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range inventory.Sources {
		if row.ID == source.ID {
			digest := sha256.Sum256(data)
			if row.Name != "listed.txt" || row.Sha256 != hex.EncodeToString(digest[:]) {
				t.Fatalf("inventory row %+v does not describe the stored source", row)
			}
			if raw, err := json.Marshal(inventory); err != nil || bytes.Contains(raw, data) {
				t.Fatal("inventory exposes source content", err)
			}
			return
		}
	}
	t.Fatal("stored source missing from the inventory")
}
