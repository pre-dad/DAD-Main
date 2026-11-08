package store

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"dad/donor-api/internal/store/queries"
	"github.com/jackc/pgx/v5/pgtype"
)

type exportDataset struct{ table, fields, permission string }

var exportDatasets = map[string]exportDataset{
	"checkouts": {
		"donor_checkouts",
		"id donor_id amount_minor currency frequency referrer_id referral_kind referral_expires_at created_at",
		"data.export",
	},
	"referral-touches": {"donor_referral_touches", "id referrer_id kind touched_at expires_at", "data.export"},
	"donors":           {"donors", "id created_at email claimed_at disabled public_name", "data.export"},
	"email-changes": {
		"donor_email_changes",
		"id donor_id old_email new_email stage expires_at created_at completed_at",
		"data.export",
	},
	"identity-events": {"donor_identity_events", "id donor_id email_change_id action created_at", "data.export"},
	"session-events":  {"donor_session_events", "id donor_id session_id action created_at", "data.export"},
	"gifts": {
		"donor_gifts",
		"id donor_id checkout_id amount_minor currency frequency paid_at period_id referrer_id referral_kind",
		"data.export",
	},
	"refunds": {"donor_refunds", "id gift_id amount_minor currency status observed_at stripe_refund_id", "data.export"},
	"refund-history": {
		"donor_refund_observations",
		"id refund_id source_event_id source_action_id status observed_at",
		"data.export",
	},
	"billing-periods": {"donor_billing_periods", "id donor_id checkout_id stripe_invoice_id due_at ends_at status", "data.export"},
	"billing-history": {
		"donor_billing_observations",
		"id period_id source_event_id status amount_minor currency billing_reason occurred_at event_created_at recorded_at",
		"data.export",
	},
	"referrals": {
		"donor_metric_projections",
		"donor_id referrer_id referral_kind first_gift_id net_minor computed_at",
		"data.export",
	},
	"referral-corrections": {
		"donor_referral_corrections",
		"id donor_id old_first_gift_id new_first_gift_id old_referrer_id new_referrer_id old_kind new_kind recorded_at rules_version",
		"data.export",
	},
	"streaks": {
		"donor_metric_projections",
		"donor_id current_streak best_streak at_risk grace_ends_at founding_awarded_at computed_at",
		"data.export",
	},
	"subscriptions": {"donor_subscriptions", "id donor_id attempt_id status period_end stops_at observed_at", "data.export"},
	"amount-changes": {
		"donor_amount_changes",
		"id donor_id checkout_id previous_minor amount_minor effective_at confirmed_at created_at",
		"data.export",
	},
	"cancellations": {"donor_cancellations", "id donor_id attempt_id confirmed_period_end completed_at created_at", "data.export"},
	"preferences": {
		"donor_preferences",
		"donor_id version referral_emails marketing_emails push_referral_converted push_streak_at_risk push_streak_milestone push_streak_lapsed push_badge_earned push_event_nearby push_event_reminder push_event_promoted rankings_opted_out network_visible volunteer",
		"data.export",
	},
	"consent-history": {
		"donor_preference_events",
		"id donor_id version referral_emails marketing_emails push_referral_converted push_streak_at_risk push_streak_milestone push_streak_lapsed push_badge_earned push_event_nearby push_event_reminder push_event_promoted rankings_opted_out network_visible volunteer consent_version source created_at",
		"data.export",
	},
	"messages": {
		"donor_messages",
		"id donor_id kind available_at completed_at attempts last_error suppressed_reason created_at",
		"data.export",
	},
	"staff-actions": {
		"donor_staff_actions",
		"id actor_id donor_id kind target_id amount_minor period_end reason status stripe_refund_id completed_at created_at",
		"data.export",
	},
	"staff": {"donor_staff", "id name email role disabled created_at", "staff.manage"},
	"audit": {"donor_staff_audit", "id actor_id action target created_at details", "audit.read"},
	"partners": {
		"donor_partner_signups",
		"id organization partner_type company_code workforce_size match_rate_bp match_cap_minor approved_at created_at",
		"partner.manage",
	},
	"content-sources": {"donor_content_sources", "id actor_id name sha256 created_at", "content.publish"},
}

// exportAsOf names, per dataset, an insert-time timestamp column that is never
// updated. A file excludes rows stamped after its request, which keeps it
// consistent as of that moment without holding a snapshot. Datasets absent here
// hold rows that change in place, so those read each row's latest state as the
// worker reaches it.
var exportAsOf = map[string]string{
	"checkouts":            "created_at",
	"donors":               "created_at",
	"email-changes":        "created_at",
	"identity-events":      "created_at",
	"session-events":       "created_at",
	"gifts":                "paid_at",
	"refund-history":       "observed_at",
	"billing-history":      "recorded_at",
	"referral-corrections": "recorded_at",
	"amount-changes":       "created_at",
	"cancellations":        "created_at",
	"consent-history":      "created_at",
	"messages":             "created_at",
	"staff-actions":        "created_at",
	"staff":                "created_at",
	"audit":                "created_at",
	"partners":             "created_at",
	"content-sources":      "created_at",
}

type ExportDatasets struct {
	Datasets []string `json:"datasets"`
}

func (s *StaffService) ExportDatasets(ctx context.Context, token string) (ExportDatasets, error) {
	actor, err := requireStaff(ctx, queries.New(s.DB), token, "data.export", time.Now().UTC())
	if err != nil {
		return ExportDatasets{}, err
	}
	names := []string{}
	permissions := staffPermissions(actor.Role)
	for name, dataset := range exportDatasets {
		if slices.Contains(permissions, dataset.permission) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return ExportDatasets{Datasets: names}, nil
}

// safeCSVCell formats a column value, quoting text a spreadsheet would
// otherwise evaluate as a formula.
func safeCSVCell(value any) (string, error) {
	var text string
	switch v := value.(type) {
	case nil:
		return "", nil
	case time.Time:
		return v.UTC().Format(time.RFC3339Nano), nil
	case [16]byte:
		return (pgtype.UUID{Bytes: v, Valid: true}).String(), nil
	case []byte:
		text = string(v)
	case map[string]any, []any:
		encoded, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		text = string(encoded)
	case string:
		text = v
	default:
		return fmt.Sprint(value), nil
	}
	trimmed := strings.TrimLeft(text, "\ufeff \t\r\n")
	if strings.HasPrefix(text, "\t") || strings.HasPrefix(text, "\r") || strings.HasPrefix(text, "\n") ||
		(len(trimmed) > 0 && strings.ContainsRune("=+-@", rune(trimmed[0]))) {
		return "'" + text, nil
	}
	return text, nil
}
