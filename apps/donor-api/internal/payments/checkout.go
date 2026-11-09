package payments

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

var (
	ErrCheckoutConflict = errors.New("checkout conflicts with an existing request")
	ErrCheckoutInput    = errors.New("invalid checkout fields")
	// ErrCheckoutThrottled reports that this email has begun too many
	// checkouts for another open one to be expired; the caller retries later.
	ErrCheckoutThrottled = errors.New("too many checkouts begun for this email; retry later")
	// ErrProcessorUnavailable reports a processor call that failed or could not
	// be made; the caller may retry the same request.
	ErrProcessorUnavailable = errors.New("payment processor is unavailable; retry the same request")
	// ErrUnconfigured reports that this deployment has no payment processor
	// configured; retrying cannot help.
	ErrUnconfigured = errors.New("payments are not configured")

	// ErrListPageCap reports that a created-range listing reached its page cap
	// before Stripe signalled has_more:false, so the range holds more objects
	// than one listing call may return. The reconcile caller splits the time
	// range and retries, so a single busy day never exceeds the cap.
	ErrListPageCap = errors.New("listing exceeded its page cap; split the range")
)

// CheckoutRequest is a signed-in donor's gift. Visitor is an anonymous referral touch.
type CheckoutRequest struct {
	Session   string
	Journey   string
	Name      string
	Zip       string
	City      string
	RequestID pgtype.UUID
	Email     string
	Amount    int64
	Frequency string
	Visitor   *string
	// NativeReturn is the origin a native client's hosted checkout returns to,
	// in place of the website's thank-you and donate pages.
	NativeReturn string
}

type CheckoutResult struct {
	CheckoutID string `json:"checkoutId"`
	URL        string `json:"url"`
}

// NativeCheckoutResult is a hosted checkout opened for a signed-in donor.
type NativeCheckoutResult struct {
	ExpiresAt time.Time `json:"expires_at"`
	SessionID string    `json:"session_id"`
	URL       string    `json:"url"`
}

type RemoteCheckout struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	Livemode *bool  `json:"livemode"`
}

type CheckoutGateway interface {
	CreateCheckout(context.Context, []byte, string) (RemoteCheckout, error)
	// ExpireCheckout closes an open hosted checkout so it can no longer be paid
	// and reports the session's resulting status: "expired", or "complete" if
	// the donor paid first. Any other outcome is an error.
	ExpireCheckout(ctx context.Context, sessionID, idempotencyKey string) (string, error)
}

// ValidateOrigin accepts an exact HTTPS origin, or plain HTTP on a loopback
// name for local runs. It must be written as browsers serialize it, in
// lowercase without a default port, since request Origin headers and passkey
// client data are compared with it byte for byte.
func ValidateOrigin(origin string) error {
	u, err := url.Parse(origin)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		(u.Scheme != "https" && !(u.Scheme == "http" && loopback(u.Hostname()))) {
		return errors.New("must be an HTTPS origin (HTTP localhost for tests)")
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" && port != map[string]string{"http": "80", "https": "443"}[u.Scheme] {
		host += ":" + port
	}
	if canonical := u.Scheme + "://" + host; origin != canonical {
		return fmt.Errorf("must be written as browsers send it: %s", canonical)
	}
	return nil
}

// LocalOrigin reports whether a valid origin names this machine.
func LocalOrigin(origin string) bool {
	u, err := url.Parse(origin)
	return err == nil && loopback(u.Hostname())
}

// loopback reports a name browsers always resolve to this machine. Every
// *.localhost name qualifies, which lets a local staff host differ from the
// public one.
func loopback(host string) bool {
	return host == "localhost" || strings.HasSuffix(host, ".localhost") || host == "127.0.0.1"
}

func ValidateRemoteCheckout(remote RemoteCheckout) error {
	u, err := url.Parse(remote.URL)
	if err != nil || !strings.HasPrefix(remote.ID, "cs_test_") || len(remote.ID) > 255 || remote.Livemode == nil || *remote.Livemode ||
		u.Scheme != "https" ||
		u.Hostname() != "checkout.stripe.com" ||
		u.User != nil ||
		(u.Port() != "" && u.Port() != "443") {
		return ErrProcessorUnavailable
	}
	return nil
}
