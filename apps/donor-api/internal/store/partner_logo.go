package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	_ "image/jpeg"
	"image/png"
	"strings"
	"time"

	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// cleanLogo validates an uploaded PNG or JPEG data URL and re-encodes it as a
// PNG, so the stored image carries no metadata or trailing bytes from the
// upload. An empty data URL removes the logo.
func cleanLogo(dataURL string) ([]byte, error) {
	if dataURL == "" {
		return nil, nil
	}
	if len(dataURL) > 350000 {
		return nil, ErrPartnerApplication
	}
	prefix, encoded, ok := strings.Cut(dataURL, ",")
	if !ok || (prefix != "data:image/png;base64" && prefix != "data:image/jpeg;base64") {
		return nil, ErrPartnerApplication
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) > 256<<10 {
		return nil, ErrPartnerApplication
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || (format != "png" && format != "jpeg") || config.Width < 1 || config.Height < 1 || config.Width > 512 ||
		config.Height > 512 {
		return nil, ErrPartnerApplication
	}
	decoded, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, ErrPartnerApplication
	}
	var out bytes.Buffer
	if err := png.Encode(&out, decoded); err != nil {
		return nil, err
	}
	if out.Len() > 1<<20 {
		return nil, ErrPartnerApplication
	}
	return out.Bytes(), nil
}

func (s PartnerDashboardService) SaveLogo(ctx context.Context, token, dataURL string) error {
	clean, err := cleanLogo(dataURL)
	if err != nil {
		return err
	}
	_, err = partnerAdminWrite(ctx, s.DB, token, func(tx pgx.Tx, actor PartnerActor, _ time.Time) (struct{}, error) {
		return struct{}{}, queries.New(tx).SetPartnerLogo(ctx, queries.SetPartnerLogoParams{ID: actor.PartnerID, LogoPng: clean})
	})
	return err
}

func (s PartnerDashboardService) Logo(ctx context.Context, token string) ([]byte, error) {
	return partnerRead(ctx, s.DB, token, func(q *queries.Queries, actor PartnerActor) ([]byte, error) {
		return q.PartnerLogo(ctx, actor.PartnerID)
	})
}

// SetLogo uploads or removes an approved partner's logo on its behalf, as part
// of enrolling it (§8). Audited.
func (s PartnerApplicationService) SetLogo(ctx context.Context, token string, partnerID pgtype.UUID, dataURL string) error {
	clean, err := cleanLogo(dataURL)
	if err != nil {
		return err
	}
	_, err = staffWrite(ctx, s.Staff, token, partnerPermission,
		func(tx pgx.Tx, actor queries.DonorStaff, now time.Time) (struct{}, error) {
			q := queries.New(tx)
			org, err := q.PartnerOrgByID(ctx, partnerID)
			if err != nil {
				return struct{}{}, err
			}
			if !org.ApprovedAt.Valid {
				return struct{}{}, pgx.ErrNoRows
			}
			if err = q.SetPartnerLogo(ctx, queries.SetPartnerLogoParams{ID: partnerID, LogoPng: clean}); err != nil {
				return struct{}{}, err
			}
			action := "partner.logo_set"
			if clean == nil {
				action = "partner.logo_removed"
			}
			return struct{}{}, auditStaff(ctx, q, actor.ID, action, partnerID.String(), nil, now)
		})
	return err
}

// Logo returns an approved partner's logo PNG for staff, empty when none is set.
func (s PartnerApplicationService) Logo(ctx context.Context, token string, partnerID pgtype.UUID) ([]byte, error) {
	return staffRead(ctx, s.Staff, token, partnerPermission,
		func(tx pgx.Tx, _ queries.DonorStaff, _ time.Time) ([]byte, error) {
			q := queries.New(tx)
			org, err := q.PartnerOrgByID(ctx, partnerID)
			if err != nil {
				return nil, err
			}
			if !org.ApprovedAt.Valid {
				return nil, pgx.ErrNoRows
			}
			return q.PartnerLogo(ctx, partnerID)
		})
}
