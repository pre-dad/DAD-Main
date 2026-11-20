// Package analytics defines the shared event taxonomy (PRD §9): the exact set
// of event names every platform may emit, and the typed properties the few
// events that carry them accept. The server validates against this before an
// event is stored, so the analytics_events table only ever holds known names
// with well-formed payloads — the schema is enforced here, not in the database.
package analytics

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrEvent is returned for an unknown event name or an invalid property payload.
var ErrEvent = errors.New("invalid analytics event")

// property describes the one typed property an event may carry: its key and the
// closed set of values it accepts. An event not present in eventProperties
// takes no properties.
type property struct {
	key    string
	values map[string]bool
}

func set(values ...string) map[string]bool {
	m := make(map[string]bool, len(values))
	for _, v := range values {
		m[v] = true
	}
	return m
}

// eventProperties is the §9 taxonomy. Every key is an allowed event name; the
// value is the typed property that event carries, or nil for an event that
// takes none. The three events with a property match §9 exactly:
// attribution_captured(source), invite_shared(channel), push_opened(type).
var eventProperties = map[string]*property{
	"app_opened":               nil,
	"role_selected":            nil,
	"signup_started":           nil,
	"signup_completed":         nil,
	"attribution_captured":     {key: "source", values: set("link", "IRL", "company")},
	"gift_started":             nil,
	"gift_completed":           nil,
	"gift_failed":              nil,
	"gift_recovered":           nil,
	"gift_lapsed":              nil,
	"amount_changed":           nil,
	"gift_cancelled":           nil,
	"account_deleted":          nil,
	"invite_shared":            {key: "channel", values: nil},
	"irl_code_shown":           nil,
	"referral_converted":       nil,
	"badge_earned":             nil,
	"milestone_reached":        nil,
	"rankings_toggled":         nil,
	"volunteer_opted_in":       nil,
	"event_viewed":             nil,
	"event_reserved":           nil,
	"event_waitlisted":         nil,
	"event_checked_in":         nil,
	"team_created":             nil,
	"team_joined":              nil,
	"partner_signup_submitted": nil,
	"partner_approved":         nil,
	"partner_report_exported":  nil,
	"push_opened":              {key: "type", values: nil},
}

// maxPropertyValue bounds a free-form property value (channel, type) so the
// payload cannot grow unbounded.
const maxPropertyValue = 64

// Known reports whether name is a §9 event name.
func Known(name string) bool {
	_, ok := eventProperties[name]
	return ok
}

// ValidateProperties checks props against name's declared property. An unknown
// event name is rejected. An event that takes no property rejects any property.
// An event with an enumerated property requires exactly that key with a value
// from its set; an event with a free-form property (channel, type) requires
// that key with a non-empty, bounded string. Extra keys are rejected so the
// payload cannot carry unvalidated data.
func ValidateProperties(name string, props map[string]string) error {
	spec, ok := eventProperties[name]
	if !ok {
		return ErrEvent
	}
	if spec == nil {
		if len(props) != 0 {
			return ErrEvent
		}
		return nil
	}
	if len(props) != 1 {
		return ErrEvent
	}
	value, ok := props[spec.key]
	if !ok {
		return ErrEvent
	}
	if spec.values != nil {
		if !spec.values[value] {
			return ErrEvent
		}
		return nil
	}
	if !validFreeForm(value) {
		return ErrEvent
	}
	return nil
}

// validFreeForm accepts printable UTF-8 of at most maxPropertyValue bytes with
// something other than whitespace in it.
func validFreeForm(value string) bool {
	return len(value) <= maxPropertyValue && utf8.ValidString(value) && strings.TrimSpace(value) != "" &&
		!strings.ContainsFunc(value, unicode.IsControl)
}
