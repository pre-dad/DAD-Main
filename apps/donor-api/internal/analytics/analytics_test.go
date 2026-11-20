package analytics

import (
	"errors"
	"testing"
)

func TestTaxonomyHasExactlyThirtyNames(t *testing.T) {
	if got := len(eventProperties); got != 30 {
		t.Fatalf("taxonomy has %d names; PRD §9 defines 30", got)
	}
	// Spot-check representative names across the lifecycle.
	for _, name := range []string{"app_opened", "attribution_captured", "push_opened", "partner_report_exported"} {
		if !Known(name) {
			t.Errorf("Known(%q) = false; want true", name)
		}
	}
	if Known("not_a_real_event") {
		t.Error("Known reported an unknown name as known")
	}
}

func TestValidateProperties(t *testing.T) {
	cases := []struct {
		name  string
		props map[string]string
		ok    bool
	}{
		// No-property events reject any property.
		{"app_opened", nil, true},
		{"app_opened", map[string]string{}, true},
		{"app_opened", map[string]string{"x": "y"}, false},
		// attribution_captured: enumerated source.
		{"attribution_captured", map[string]string{"source": "link"}, true},
		{"attribution_captured", map[string]string{"source": "IRL"}, true},
		{"attribution_captured", map[string]string{"source": "company"}, true},
		{"attribution_captured", map[string]string{"source": "other"}, false},
		{"attribution_captured", map[string]string{"channel": "link"}, false},
		{"attribution_captured", nil, false},
		// invite_shared: free-form but required, non-empty, bounded channel.
		{"invite_shared", map[string]string{"channel": "sms"}, true},
		{"invite_shared", map[string]string{"channel": ""}, false},
		{"invite_shared", map[string]string{"type": "sms"}, false},
		{"invite_shared", nil, false},
		// push_opened: free-form type.
		{"push_opened", map[string]string{"type": "STREAK_AT_RISK"}, true},
		{"push_opened", map[string]string{"type": "x", "extra": "y"}, false},
		{"invite_shared", map[string]string{"channel": "   "}, false},
		{"invite_shared", map[string]string{"channel": "a\x00b"}, false},
		{"invite_shared", map[string]string{"channel": "a\nb"}, false},
		{"invite_shared", map[string]string{"channel": "\xff"}, false},
		{"invite_shared", map[string]string{"channel": "café"}, true},
		// Unknown event.
		{"nope", nil, false},
	}
	for _, c := range cases {
		err := ValidateProperties(c.name, c.props)
		if c.ok && err != nil {
			t.Errorf("ValidateProperties(%q, %v) = %v; want ok", c.name, c.props, err)
		}
		if !c.ok && !errors.Is(err, ErrEvent) {
			t.Errorf("ValidateProperties(%q, %v) = %v; want ErrEvent", c.name, c.props, err)
		}
	}
}

func TestOverlongPropertyRejected(t *testing.T) {
	long := make([]byte, maxPropertyValue+1)
	for i := range long {
		long[i] = 'a'
	}
	if err := ValidateProperties("push_opened", map[string]string{"type": string(long)}); !errors.Is(err, ErrEvent) {
		t.Errorf("overlong value error = %v; want ErrEvent", err)
	}
}
