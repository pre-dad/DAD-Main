package store

import (
	"strconv"

	"dad/donor-api/internal/payments"
)

// publicSite builds every URL the store hands to donors, Stripe and email
// from the validated public origin. They are web pages: a native app opens
// them as universal links rather than through a separate scheme, so this is
// the deep-link map clients must route. Tokens travel in the fragment, which
// browsers never send to a server.
type publicSite string

func site(origin string) (publicSite, error) {
	if err := payments.ValidateOrigin(origin); err != nil {
		return "", err
	}
	return publicSite(origin), nil
}

func (s publicSite) referral(code string) string    { return string(s) + "/r/" + code }
func (s publicSite) irlReferral(code string) string { return s.referral(code) + "?via=irl" }

// ValidReturnPath reports whether a sign-in link may return a donor to next.
func ValidReturnPath(next string) bool {
	return next == "/donate" || next == "/account/payment-method" || next == "/account/dashboard"
}

// claim is a sign-in link, which returns the donor to next if it is set.
func (s publicSite) claim(token, next string) string {
	link := string(s) + "/account/claim#token=" + token
	if next != "" {
		link += "&next=" + next
	}
	return link
}

func (s publicSite) partnerSignIn(token string) string {
	return string(s) + "/partner/accept#token=" + token
}

func (s publicSite) emailChange(token string) string {
	return string(s) + "/account/email-change#token=" + token
}

// Pages a push notification opens, as paths for the app to route.
const (
	dashboardPath     = "/account/dashboard"
	paymentMethodPath = "/account/payment-method"
)

func (s publicSite) dashboard() string      { return string(s) + dashboardPath }
func (s publicSite) event(id string) string { return string(s) + "/events/" + id }
func (s publicSite) profile() string        { return string(s) + "/account/profile" }
func (s publicSite) paymentMethod() string  { return string(s) + paymentMethodPath }
func (s publicSite) donate() string         { return string(s) + "/donate" }

// checkoutReturn is where hosted checkout returns a donor; Stripe substitutes
// the session placeholder.
func (s publicSite) checkoutReturn(amount int64, frequency string) string {
	return string(
		s,
	) + "/donate/thanks?session_id={CHECKOUT_SESSION_ID}&amount_minor=" + strconv.FormatInt(
		amount,
		10,
	) + "&frequency=" + frequency
}

// nativeReturnPath is where a native client's hosted checkout returns. The page
// answers a browser with a short "return to the app" notice; the app watches
// for the address itself or, once its association covers the path, opens from it.
const nativeReturnPath = "/app/return"

func (s publicSite) nativeCheckoutReturn() string {
	return string(s) + nativeReturnPath + "?result=success&session_id={CHECKOUT_SESSION_ID}"
}

func (s publicSite) nativeCheckoutCancel() string {
	return string(s) + nativeReturnPath + "?result=cancel"
}
