package httpapi

import "net/http"

// auth names the credential a route acts on. The OpenAPI contract test checks
// each operation's documented security against it.
type auth string

const (
	applicationAuth auth = "application"
	noAuth          auth = "none"
	donorAuth       auth = "donor"   // browser cookie, or bearer token from the donor app
	partnerAuth     auth = "partner" // browser cookie, or bearer token from the app
	staffAuth       auth = "staff"   // browser cookie only
	stripeAuth      auth = "stripe"  // Stripe-Signature over the raw body
)

type route struct {
	pattern string
	auth    auth
	handler http.HandlerFunc
}

// routes is the complete API surface. Reads are unguarded because they cannot
// change state. Mutations pass the Origin and rate-limit guard: donor and
// public routes admit the public origin, and the native donor app on donor
// account routes and link capture only, of which app entry routes admit
// nothing else; staff
// routes admit only the staff origin, and refuse any request a browser reports
// as started elsewhere. The Stripe webhook is authenticated by its signature
// instead.
func (a *api) routes() []route {
	return []route{
		a.public("GET /health/live", a.live),
		a.public("GET /health/ready", a.ready),
		a.public("GET /donor/public/spend", a.overallSpend),
		{"POST /donor/stripe/webhook", stripeAuth, a.stripeWebhook},
		a.visitor("POST /donor/checkouts", a.beginCheckout),
		a.public("GET /donor/referrals/{code}", a.referrer),
		a.capture("POST /donor/referrals/{code}", a.captureReferral),
		a.capture("POST /donor/company/{code}", a.captureCompany),
		{"POST /analytics/events", noAuth, a.guarded(a.Origin, analyticsLimit, true, a.ingestAnalytics)},
		a.visitor("POST /donor/public/partner-apply", a.partnerApply),
		a.appVisitor("POST /donor/partner/applications", a.applyWithReceipt),
		{"GET /donor/partner/application-status", applicationAuth, signedIn(applicationSession, a.applicationStatus)},
		a.partnerEntry("POST /donor/partner/sign-in", a.partnerSignIn),
		a.partnerEntry("POST /donor/partner/accept-invite", a.partnerAccept),

		a.donorEntry("POST /donor/signup", a.signup),
		a.donorEntry("POST /donor/password/sign-in", a.passwordSignIn),
		a.donorWrite("POST /donor/me/password", a.setPassword),
		a.donorEntry("POST /donor/login", a.login),
		a.donorEntry("POST /donor/claim", a.claim),
		a.donorEntry("POST /donor/passkeys/sign-in/options", a.beginDonorPasskeySignIn),
		a.donorEntry("POST /donor/passkeys/sign-in", a.finishDonorPasskeySignIn),
		a.donorEntry("POST /donor/phone/sign-in/options", a.beginPhoneSignIn),
		a.donorEntry("POST /donor/phone/sign-in", a.finishPhoneSignIn),
		a.donorEntry("POST /donor/oidc/{provider}/sign-in", a.oidcSignInHandler),
		a.appEntry("POST /donor/refresh", a.refresh),
		a.donorEntry("POST /donor/email-change/confirm", a.confirmEmailChange),
		{"POST /donor/logout", donorAuth, a.guarded(a.Origin, accountLimit, true, a.logout)},
		a.donorWrite("POST /donor/logout-all", a.logoutAll),
		a.donorWrite("POST /donor/me/email-change", a.requestEmailChange),

		a.donorRead("GET /donor/me/dashboard", a.dashboard),
		a.donorWrite("POST /donor/me/employer", a.employeeEnroll),
		a.donorRead("GET /donor/me/team", a.employeeTeam),
		a.donorWrite("POST /donor/me/team", a.employeeJoinTeam),
		a.donorRead("GET /donor/me/profile", a.profile),
		a.donorWrite("POST /donor/me/profile", a.saveProfile),
		a.donorWrite("POST /donor/me/delete", a.deleteAccount),
		a.donorRead("GET /donor/me/sessions", a.sessions),
		a.donorWrite("POST /donor/me/sessions/{session_id}/revoke", a.signOutSession),
		a.donorWrite("POST /donor/me/sessions/revoke-others", a.signOutOthers),
		a.donorWrite("POST /donor/me/preferences", a.savePreferences),
		a.appWrite("POST /donor/me/push-device", a.registerPushDevice),
		a.appWrite("POST /donor/me/push-device/unregister", a.unregisterPushDevice),
		a.appWrite("POST /donor/me/company-touch", a.bindCompanyTouch),
		a.donorRead("GET /donor/me/conversions", a.conversions),
		a.donorRead("GET /donor/me/today-initiative", a.todayInitiative),
		a.donorRead("GET /donor/me/badges", a.donorBadges),
		a.donorRead("GET /donor/me/network", a.donorNetwork),
		a.donorRead("GET /donor/me/events", a.events),
		a.donorRead("GET /donor/me/events/invite-only", a.inviteOnlyEvents),
		a.donorRead("GET /donor/me/events/{event_id}", a.donorEventDetail),
		a.donorRead("GET /donor/me/events/{event_id}/calendar.ics", a.donorEventCalendar),
		a.donorWrite("POST /donor/me/events/{event_id}/reserve", a.reserveEvent),
		a.donorWrite("POST /donor/me/events/{event_id}/cancel", a.cancelEvent),
		a.donorWrite("POST /donor/me/events/check-in", a.checkInEvent),
		a.donorRead("GET /donor/me/sharing", a.sharing),
		a.donorWrite("POST /donor/me/sharing", a.saveSharing),
		a.donorRead("GET /donor/me/referral-code.svg", a.referralCode),
		a.donorRead("GET /donor/me/passkeys", a.listDonorPasskeys),
		a.donorWrite("POST /donor/me/passkeys/register/options", a.beginDonorPasskeyRegister),
		a.donorWrite("POST /donor/me/passkeys/register", a.finishDonorPasskeyRegister),
		a.donorWrite("POST /donor/me/passkeys/{passkey_id}/delete", a.deleteDonorPasskey),
		a.donorRead("GET /donor/me/gifts", a.gifts),
		a.donorRead("GET /donor/me/receipts/{gift_id}", a.receipt),
		a.donorRead("GET /donor/me/annual-summary/{year}", a.annualSummary),
		a.donorRead("GET /donor/me/wallet", a.walletProviders),
		a.donorWrite("POST /donor/me/wallet/apple", a.applePass),
		a.donorWrite("POST /donor/me/wallet/google", a.googlePass),

		a.donorRead("GET /donor/me/recurring", recurringRead(a, RecurringSource.Details)),
		a.donorRead("GET /donor/me/recurring/amount", recurringRead(a, RecurringSource.AmountOptions)),
		a.donorRead("GET /donor/me/payment-method", recurringRead(a, RecurringSource.PaymentMethodStatus)),
		a.appWrite("POST /donor/me/recurring/checkout", a.startRecurringCheckout),
		a.donorWrite("POST /donor/me/recurring/cancel", recurringCommand(a, cancelRecurring)),
		a.donorWrite("POST /donor/me/recurring/amount", recurringCommand(a, changeAmount)),
		a.donorWrite("POST /donor/me/payment-method/setup", a.beginPaymentUpdate),

		a.staffChallenge("POST /donor/staff/sign-in/options", a.beginStaffSignIn),
		a.staffEntry("POST /donor/staff/sign-in", a.staffSignIn),
		a.staffChallenge("POST /donor/staff/activate/options", a.beginStaffActivation),
		a.staffEntry("POST /donor/staff/activate", a.staffActivate),
		{"POST /donor/staff/sign-out", staffAuth, sameOrigin(a.guarded(a.StaffOrigin, staffLimit, false, a.staffSignOut))},
		a.staffRead("GET /donor/staff/me", a.staffMe),
		a.staffRead("GET /donor/staff/accounts", a.staffAccounts),
		a.staffWrite("POST /donor/staff/accounts", a.inviteStaff),
		a.staffWrite("POST /donor/staff/accounts/{staff_id}/disable", a.changeStaffAccess("disable")),
		a.staffWrite("POST /donor/staff/accounts/{staff_id}/recovery", a.changeStaffAccess("recovery")),
		a.staffWrite("POST /donor/staff/accounts/{staff_id}/role", a.changeStaffAccess("role")),

		a.staffRead("GET /donor/staff/donors", a.searchDonors),
		a.staffRead("GET /donor/staff/donors/{donor_id}", a.donorDetail),
		a.staffRead("GET /donor/staff/donors/{donor_id}/messages", a.donorMessages),
		a.staffRead("GET /donor/staff/donors/{donor_id}/actions", a.donorActions),
		a.staffRead("GET /donor/staff/donors/{donor_id}/actions/pending", a.pendingAction),
		a.staffRead("GET /donor/staff/audit", a.auditEvents),
		a.staffRead("GET /donor/staff/failed-payments", a.failedPayments),
		a.staffRead("GET /donor/staff/deletions", a.accountDeletions),
		a.staffRead("GET /donor/staff/report", a.report),
		a.staffWrite("POST /donor/staff/donors/{donor_id}/claim-email", a.requestClaim),
		a.staffWrite("POST /donor/staff/donors/{donor_id}/recognition/reset", a.resetStreak),
		a.staffWrite("POST /donor/staff/donors/{donor_id}/recognition/recompute", a.recomputeRecognition),
		a.staffWrite("POST /donor/staff/donors/{donor_id}/badges/grant", a.grantBadge),
		a.staffWrite("POST /donor/staff/donors/{donor_id}/badges/revoke", a.revokeBadge),
		a.staffWrite("POST /donor/staff/messages/{message_id}/resend", a.resendMessage),

		a.staffRead("GET /donor/staff/donors/{donor_id}/gifts/{gift_id}/refund", a.refundQuote),
		a.staffWrite("POST /donor/staff/donors/{donor_id}/gifts/{gift_id}/refund", a.refund),
		a.staffRead("GET /donor/staff/donors/{donor_id}/cancellation", a.cancellationQuote),
		a.staffWrite("POST /donor/staff/donors/{donor_id}/cancellation", a.cancelSubscription),

		a.staffRead("GET /donor/staff/exports", a.exportDatasets),
		a.staffWrite("POST /donor/staff/exports/jobs", a.requestExport),
		a.staffRead("GET /donor/staff/exports/jobs", a.exportJobs),
		a.staffRead("GET /donor/staff/exports/jobs/{job_id}", a.exportJob),
		a.staffRead("GET /donor/staff/exports/jobs/{job_id}/file", a.downloadExport),

		a.staffRead("GET /donor/staff/content", a.contentInventory),
		a.staffRead("GET /donor/staff/content/rules", a.recognitionRules),
		a.staffWrite("POST /donor/staff/content/rules", a.setRecognitionRules),
		a.staffRead("GET /donor/staff/content/wording", a.recognitionWording),
		a.partnerAppWrite("POST /donor/partner/push-device", a.registerPartnerPushDevice),
		a.partnerAppWrite("POST /donor/partner/push-device/unregister", a.unregisterPartnerPushDevice),
		a.partnerRead("GET /donor/partner/overview", a.partnerOverview),
		a.partnerRead("GET /donor/partner/initiatives", a.partnerInitiatives),
		a.partnerRead("GET /donor/partner/spend", a.partnerSpend),
		a.partnerRead("GET /donor/partner/users", a.partnerUsers),
		a.partnerRead("GET /donor/partner/exports", a.partnerExports),
		a.partnerRead("GET /donor/partner/our-people", a.employeeOurPeople),
		a.partnerRead("GET /donor/partner/teams", a.employeeTeams),
		a.partnerWrite("POST /donor/partner/workforce", a.employeeWorkforce),
		a.partnerWrite("POST /donor/partner/profile", a.partnerSaveProfile),
		a.partnerReportRoute("GET /donor/partner/report.pdf", a.partnerReport),
		a.partnerRead("GET /donor/partner/reports/{report_id}/report.pdf", a.partnerReportArtifact),
		a.partnerRead("GET /donor/partner/logo", a.partnerLogo),
		a.partnerWrite("POST /donor/partner/logo", a.partnerSaveLogo),
		a.partnerRead("GET /donor/partner/company-code.svg", a.partnerCompanyCode),
		a.partnerWrite("POST /donor/partner/users", a.partnerInviteUser),
		a.partnerWrite("POST /donor/partner/users/{userID}/remove", a.partnerRemoveUser),
		a.partnerWrite("POST /donor/partner/sign-out", a.partnerSignOut),
		a.staffRead("GET /donor/staff/partners", a.staffPartners),
		a.staffRead("GET /donor/staff/partners/pending", a.staffPartnerPending),
		a.staffRead("GET /donor/staff/partners/{partnerID}", a.staffPartnerDetail),
		a.staffWrite("POST /donor/staff/partners/{partnerID}/users", a.staffPartnerInviteUser),
		a.staffRead("GET /donor/staff/partners/{partnerID}/logo", a.staffPartnerLogo),
		a.staffWrite("POST /donor/staff/partners/{partnerID}/logo", a.staffPartnerSaveLogo),
		a.staffWrite("POST /donor/staff/partners/{partnerID}/users/{userID}/remove", a.staffPartnerRemoveUser),
		a.staffWrite("POST /donor/staff/partners/{appID}/approve", a.staffPartnerApprove),
		a.staffWrite("POST /donor/staff/partners/{appID}/reject", a.staffPartnerReject),
		a.staffWrite("POST /donor/staff/partners/{partnerID}/link", a.staffPartnerLink),
		a.staffWrite("POST /donor/staff/partners/{partnerID}/match", a.staffPartnerMatch),
		a.staffRead("GET /donor/staff/partners/{partnerID}/match-status", a.staffPartnerMatchStatus),
		a.staffWrite("POST /donor/staff/partners/{partnerID}/match-payment", a.staffPartnerMatchPayment),
		a.staffWrite("POST /donor/staff/content/sources", a.uploadSource),
		a.staffRead("GET /donor/staff/content/sources/{source_id}", a.downloadSource),
		a.staffWrite("POST /donor/staff/content/wording", a.setRecognitionWording),
		a.staffWrite("POST /donor/staff/founding-claims", a.importFoundingClaims),

		a.staffRead("GET /donor/staff/initiatives", a.staffInitiatives),
		a.staffWrite("POST /donor/staff/initiatives", a.createInitiative),
		a.staffRead("GET /donor/staff/initiatives/{initiative_id}", a.staffInitiative),
		a.staffWrite("POST /donor/staff/initiatives/{initiative_id}/rotation", a.setInitiativeRotation),
		a.staffWrite("POST /donor/staff/initiatives/{initiative_id}", a.updateInitiative),
		a.staffWrite("POST /donor/staff/initiatives/{initiative_id}/publish", a.publishInitiative),
		a.staffWrite("POST /donor/staff/initiatives/{initiative_id}/metrics/{position}", a.setInitiativeMetric),
		a.staffWrite("POST /donor/staff/initiatives/{initiative_id}/metrics/{position}/publish", a.publishInitiativeMetric),
		a.staffWrite("POST /donor/staff/initiatives/{initiative_id}/metrics/{position}/delete", a.deleteInitiativeMetric),
		a.staffWrite("POST /donor/staff/initiatives/{initiative_id}/spend", a.setInitiativeSpend),
		a.staffWrite("POST /donor/staff/initiatives/{initiative_id}/spend/publish", a.publishInitiativeSpend),
		a.staffWrite("POST /donor/staff/initiatives/{initiative_id}/spend/delete", a.deleteInitiativeSpend),
		a.staffRead("GET /donor/staff/events", a.staffEvents),
		a.staffWrite("POST /donor/staff/events", a.createEvent),
		a.staffRead("GET /donor/staff/events/{event_id}", a.staffEvent),
		a.staffWrite("POST /donor/staff/events/{event_id}/cancel", a.cancelStaffEvent),
		a.staffRead("GET /donor/staff/events/{event_id}/check-in.svg", a.eventCheckInQR),
		a.staffWrite("POST /donor/staff/events/{event_id}/attendees/{donor_id}/check-in", a.staffEventCheckIn),
		a.staffWrite("POST /donor/staff/events/{event_id}", a.updateEvent),
		a.staffWrite("POST /donor/staff/events/{event_id}/publish", a.publishEvent),
		a.staffWrite("POST /donor/staff/events/{event_id}/guests/{donor_id}", a.addEventGuest),
		a.staffWrite("POST /donor/staff/events/{event_id}/guests/{donor_id}/remove", a.removeEventGuest),
		a.staffWrite("POST /donor/staff/events/{event_id}/check-in-token", a.eventCheckInToken),
		a.public("GET /initiatives", a.publishedInitiatives),
		a.public("GET /initiatives/{slug}", a.publishedInitiative),
		a.public("GET /initiatives/{initiative_id}/spend", a.initiativeSpend),
		a.public("GET /events/{event_id}", a.eventDetail),
		a.public("GET /events/{event_id}/calendar.ics", a.eventCalendar),
	}
}

// public routes are anonymous reads.
func (a *api) public(pattern string, handler http.HandlerFunc) route {
	return route{pattern, noAuth, handler}
}

// visitor routes are anonymous browser mutations on the public site.
func (a *api) visitor(pattern string, handler http.HandlerFunc) route {
	return route{pattern, noAuth, a.guarded(a.Origin, visitorLimit, false, handler)}
}

// appVisitor is visitor for a route the installed apps also reach, with the
// same anonymous-visitor budget: a native request carries no Origin or cookie
// and gets its result in the body.
func (a *api) appVisitor(pattern string, handler http.HandlerFunc) route {
	return route{pattern, noAuth, a.guarded(a.Origin, visitorLimit, true, handler)}
}

// capture routes record an anonymous referral or company-code visit, in a
// browser cookie or, for the app opening the same link, a token it keeps.
func (a *api) capture(pattern string, handler http.HandlerFunc) route {
	return route{pattern, noAuth, a.guarded(a.Origin, visitorLimit, true, handler)}
}

// donorEntry routes exchange an emailed proof, from a browser or the app.
func (a *api) donorEntry(pattern string, handler http.HandlerFunc) route {
	return route{pattern, noAuth, a.guarded(a.Origin, authLimit, true, handler)}
}

// appEntry routes renew the app's credentials. Only the app may hold those,
// so a browser is refused even from the public origin, and they take the
// account budget: refresh tokens cannot be guessed, and sign-ins at a busy
// event must not exhaust the budget refreshes need.
func (a *api) appEntry(pattern string, handler http.HandlerFunc) route {
	return route{pattern, noAuth, appOnly(a.guarded(a.Origin, accountLimit, true, handler))}
}

func (a *api) donorRead(pattern string, handler sessionHandler) route {
	return route{pattern, donorAuth, signedIn(donorSession, handler)}
}

func (a *api) donorWrite(pattern string, handler sessionHandler) route {
	return route{pattern, donorAuth, a.guarded(a.Origin, accountLimit, true, signedIn(donorSession, handler))}
}

// appWrite routes act on the app installation behind the session, which a
// browser does not have. The session is checked first, so an anonymous
// request is unauthenticated, as on every other donor route.
func (a *api) appWrite(pattern string, handler sessionHandler) route {
	return route{
		pattern,
		donorAuth,
		a.guarded(a.Origin, accountLimit, true, signedIn(donorSession, func(w http.ResponseWriter, r *http.Request, session string) {
			if !native(r) {
				writeError(w, r, http.StatusForbidden, "Request origin is not allowed", nil)
				return
			}
			handler(w, r, session)
		})),
	}
}

// staffChallenge routes start passkey ceremonies. A page asks for one before
// anyone acts, so they take the staff budget; answering one takes the
// stricter sign-in budget.
func (a *api) staffChallenge(pattern string, handler http.HandlerFunc) route {
	return route{pattern, noAuth, sameOrigin(a.guarded(a.StaffOrigin, staffLimit, false, handler))}
}

// staffEntry routes answer passkey ceremonies; the back office is browser-only.
func (a *api) staffEntry(pattern string, handler http.HandlerFunc) route {
	return route{pattern, noAuth, sameOrigin(a.guarded(a.StaffOrigin, authLimit, false, handler))}
}

func (a *api) staffRead(pattern string, handler sessionHandler) route {
	return route{pattern, staffAuth, sameOrigin(signedIn(a.staffSession, handler))}
}

func (a *api) staffWrite(pattern string, handler sessionHandler) route {
	return route{pattern, staffAuth, sameOrigin(a.guarded(a.StaffOrigin, staffLimit, false, signedIn(a.staffSession, handler)))}
}

// partnerEntry routes request and exchange the partner portal's emailed
// sign-in links, from a browser or the app.
func (a *api) partnerEntry(pattern string, handler http.HandlerFunc) route {
	return route{pattern, noAuth, a.guarded(a.Origin, authLimit, true, handler)}
}

// partnerRead and partnerWrite serve the partner portal on the public origin
// and in the app, authenticated by a partner session, never a donor or staff
// one (P-5).
func (a *api) partnerRead(pattern string, handler sessionHandler) route {
	return route{pattern, partnerAuth, signedIn(partnerSession, handler)}
}

// partnerReportRoute serves a read that retains a report, so like a staff read
// it refuses a request another site started: the Lax partner cookie would
// otherwise ride along on a link or form from anywhere.
func (a *api) partnerReportRoute(pattern string, handler sessionHandler) route {
	return route{pattern, partnerAuth, sameOrigin(signedIn(partnerSession, handler))}
}

func (a *api) partnerWrite(pattern string, handler sessionHandler) route {
	return route{pattern, partnerAuth, a.guarded(a.Origin, accountLimit, true, signedIn(partnerSession, handler))}
}

// sameOrigin refuses a request the browser reports, in Sec-Fetch-Site, as
// started by another host. The staff and public hosts are the same site, so
// the SameSite=Strict staff cookie still rides on a link followed from the
// public host, which could otherwise start a staff read with an effect, such
// as an audited export download. Typed addresses and bookmarks report none.
func sameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			writeError(w, r, http.StatusForbidden, "Request origin is not allowed", nil)
			return
		}
		next(w, r)
	}
}

func appOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !native(r) {
			writeError(w, r, http.StatusForbidden, "Request origin is not allowed", nil)
			return
		}
		next(w, r)
	}
}

// sessionHandler serves a signed-in request. The session is only a claim: the
// store authenticates it, and for staff checks the route's named permission
// against current staff state.
type sessionHandler func(w http.ResponseWriter, r *http.Request, session string)

// signedIn rejects requests without a session before they reach storage.
func signedIn(session func(*http.Request) string, next sessionHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := session(r)
		if token == "" {
			writeError(w, r, http.StatusUnauthorized, "Sign in required", nil)
			return
		}
		next(w, r, token)
	}
}

func (a *api) partnerAppWrite(pattern string, handler sessionHandler) route {
	return a.partnerWrite(pattern, func(w http.ResponseWriter, r *http.Request, token string) {
		if !native(r) {
			writeError(w, r, http.StatusForbidden, "Request origin is not allowed", nil)
			return
		}
		handler(w, r, token)
	})
}
