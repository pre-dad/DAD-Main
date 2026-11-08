# Shared mobile foundation

DAD-Main owns donor business rules, the canonical OpenAPI contract, design values, and
supported donor links. Mobile applications consume a pinned release of these interfaces.
Their screens and platform interactions are separate from the Go financial domain.

## Versioned bundle

`contracts/mobile/version.json` versions the bundle independently from backend releases.
`make bundle-mobile MOBILE_BUNDLE_DIR=/tmp/dad-foundation` creates:

- `openapi.json`: the complete canonical API.
- `mobile.openapi.json`: native-admitted mutations, every bearer-authenticated read (donor
  account, partner portal, application status), the public initiative and event reads, and
  liveness, derived from the canonical API without rewriting schemas or response variants.
- `swift.openapi.json`: the mobile API adapted for the pinned Apple generator, with
  equivalent nullable object/scalar states and an explicit literal-null type override.
- `DADNullValue.swift`: the strict Codable implementation for that override.
- `tokens.json`: platform-neutral design values, with `schema_version`.
- `tokens.ts`, `DADTokens.swift`, `DADTokens.kt`: generated platform values.
- `links.json`: approved account, proof, referral, and notification destinations.
- `manifest.json`: bundle version, source repository and Git revision, and SHA-256 of every
  other file.

The mobile contract is an integration surface, not a decision to expose every operation
in the first mobile product. Backend authorization remains authoritative. Native requests
call the API host directly with `X-DAD-Client: native`, no cookies or Origin, and a bearer
access token when authenticated. Browser-only giving and staff operations stay on the web.

After checks pass on the release commit, tag it `mobile-foundation-v<version>` (using the
version in the file, for example `mobile-foundation-v2.11.0`) and push that tag. The
mobile-foundation workflow verifies the tag matches the declared version and publishes the
individual files as GitHub Release assets.
Existing release assets are never overwritten. The workflow also retains a candidate
bundle as an Actions artifact on normal branch and PR builds; those artifacts are not
released versions. A release must be built from committed, clean sources.

Each app records the exact release tag and manifest hash, vendors that release's files,
and verifies both the manifest and file hashes before generation. Updating is an explicit
reviewed change, not a fetch of `dev` or `latest` during a build. CI can regenerate from
the vendored contract without network access to this repository. A maintainer
can obtain a release with:

```sh
gh release download mobile-foundation-v2.11.0 --repo pre-dad/DAD-Main --dir /tmp/dad-foundation
```

Use patch versions for equivalent packaging corrections, minor versions for compatible
additions, and major versions for breaking contract/token/link changes. A mobile release
pins its dependency; changing the backend must preserve supported installed clients.
Breaking field removals, type changes, and newly required request fields require a
coordinated compatibility plan. Do not invent financial values for unknown server states.

## Pending for the next release: OIDC nonce

`oidcSignIn` gains an optional `nonce` (1 to 256 characters). It is not in a cut bundle
yet; 2.11.0 clients keep working because the field is optional. A native client should
generate a random value per attempt and send it:

- Sign in with Apple: give `ASAuthorizationAppleIDRequest.nonce` the lowercase hex
  SHA-256 of the value and send the raw value as `nonce`. Apple puts the hash in the
  ID token, and the server accepts it.
- Google (Credential Manager): pass the raw value as the request nonce and send the same
  value as `nonce`.

A mismatch, or a token without a `nonce` claim, fails like any unverifiable token (401).
The server makes the field mandatory only when `DONOR_OIDC_REQUIRE_NONCE=true`, which is
for after the apps that send it are the oldest in use.

## Compatible additions in version 2.11

Version 2.11 lets an installed app submit a partner application and read its status.
Before it, `applyWithReceipt` was browser-only, so an app could not obtain a receipt and
`applicationStatus` had nothing to read. No existing field, status or enum value changes.

- `applyWithReceipt` (`POST /donor/partner/applications`) admits native requests. The
  receipt is chosen by the app, never issued by the server: generate 32 random bytes,
  encode them as unpadded base64url (43 characters) and keep them in secure storage
  before sending.

  ```
  POST /donor/partner/applications
  X-DAD-Client: native
  Idempotency-Key: <version 4 UUID>
  Content-Type: application/json

  {"application": {...}, "receipt": "<43 characters>"}
  ```

  A success is `200 {"received": true}` with no `Set-Cookie`; the receipt is not echoed.
  Send no `Origin` or `Cookie` header: a request carrying either is treated as a browser
  request and refused as such, as on the other native entry routes. Retry a failed attempt
  with the same `Idempotency-Key` and the same receipt. The route takes the visitor rate
  budget (60 per minute per client address); a 429 carries `Retry-After`.
- Read the status with `applicationStatus` (`GET /donor/partner/application-status`),
  sending `X-DAD-Client: native` and `Authorization: Bearer <receipt>`. A receipt the
  server does not know, or one older than 30 days, answers 401. Keep the receipt while the
  application is pending and show the status card from it; discard it once the application
  is decided and shown, or on a 401.
  Keep it apart from donor and partner session tokens and send it only to this route.
- `partnerRetainedReport` documents its `404` (an unknown, foreign or malformed report
  ID); the staff route `removeEventGuest`, outside the bundle, documents its 404 too.
- The `RateLimitedByIngress` 429 now describes the `Problem` body and `Retry-After` that
  the ingress sends. Keep tolerating an empty or non-JSON 429 from an older edge.

## Compatible additions in version 2.10

Version 2.10 lets the apps carry referral and company-code attribution from a link that
opens the app, as a browser's cookies carry it on the web. Every addition is optional or
new; no existing field, status or enum value changes.

- `captureReferral` and `captureCompany` (`POST /donor/referrals/{code}`,
  `POST /donor/company/{code}`) admit native requests. Call them when the app opens a
  `/r/{code}` or `/c/{code}` link, sending the optional request `token` kept from the last
  capture of the same kind. The response adds `token` (string or `null`, native responses
  only): keep a string, and discard any kept token on `null`. A token lasts 30 days from
  its latest touch; the latest valid link wins.
- `startRecurringCheckout` takes an optional `referral_token`: the kept referral token.
  The referrer is credited exactly as on the web, so a token past its window or naming the
  donor themself is ignored rather than refused. Repeat it on a retry under the same
  `Idempotency-Key`; an open checkout is handed back only if it credits the same referrer.
- `bindCompanyTouch` (`POST /donor/me/company-touch`, native only) takes the kept company
  token after sign-in and returns `bound`. It links the donor to the employer as signing in
  does on the web, and never replaces an existing one. Discard the token afterwards.
- Version 2.10.1 changes no schema. `links.json` sets `referrals_open_web` to `false`:
  from 2.10 the apps open `/r/{code}` and `/c/{code}` links themselves, as the association
  files already let them.

## Compatible additions in version 2.9

Version 2.9 puts the city on the Top 50 City badge. The badge `code` stays `CITY50`; it is
the stable identifier, and the badge's city varies (PRD Appendix A, "Top 50 Chicago"). No
request, existing field or enum value changes. Both fields are on every `BadgeView` from
`donorBadges`.

- `city` (required, string or `null`) is the city the badge was earned in. It is recorded
  when the badge is earned, so it stays that city if the donor later moves. It is `null` on
  every other badge, on a `CITY50` not yet earned, and on one staff granted. A `CITY50` earned
  before 2.9 carries the donor's city as of the 2.9 release.
- `display_title` (required, string) is the title to show. For an earned city badge with a
  city it reads `Top 50 in {city}` (for example `Top 50 in Chicago`, following the badge's
  threshold if staff change it); for every other badge it equals `title`. Show it on the
  earned badge and keep `title` (`Top 50 City`) for the unearned one. A client that composes
  its own copy should use `Top 50 in {city}`, and fall back to `title` when `city` is `null`.

## Compatible additions in version 2.8

Version 2.8 adds the donor's referral code to the sharing reads and documents how to save
one preference. No request, existing field or enum value changes.

- `Sharing.referral_code` (required, string or `null`) is the code inside `link` and
  `irl_link`, for display and copying. It is `null` exactly when they are (no public name
  chosen yet) and changes when `saveSharing` replaces the code. It is on `sharing` and
  `saveSharing`. The invite paths are `/r/{code}` and `/r/{code}?via=irl` on the public
  origin; use `link` and `irl_link` as given rather than building them.
- `savePreferences` is optimistic concurrency by design: every save is a numbered record in
  the consent history, so it names the profile `version` it replaces and answers `409` when
  that is stale. To change one setting, read the profile, send its `version` and the six
  required fields from `preferences` unchanged, and change only the one you mean to; leave
  the optional fields out to keep their saved values.

## Compatible additions in version 2.7

Version 2.7 closes the gaps native event screens hit. Every addition is a new response
field; no request, existing field or enum value changes.

- `EventView.time_zone` (required, string) is the venue's IANA zone, for example
  `America/Chicago`. It is on the list, detail and invite-only reads. `starts_at` and
  `ends_at` stay instants; format them in this zone, not the device's, so a donor sees the
  time printed at the venue (PRD E-3). Staff set it on the event (`time_zone` on the staff
  event input and reads). Left empty it takes the zone of the event's ZIP, else
  `America/New_York`, and an unknown name is refused with `422`. Events that existed before
  2.7 read `America/New_York` until staff correct them.
- `EventView.checked_in_at` (required, date-time or `null`) is when the signed-in donor
  checked in. `reservation_status` stays `RESERVED` after check-in, so use `checked_in_at`
  to show the checked-in state on a later read. It is `null` before check-in and on the
  anonymous read.
- The `checkInEvent` token is documented: exactly 66 characters, `<22 base64url>.<43
  base64url>` over `A-Z a-z 0-9 - _` plus one `.`, the event id and its signature. Pass
  the QR's text through unchanged. Any other length or shape answers `422`, the same as a
  token for an event outside its check-in window or for a donor without a seat.
- Retrying is safe without an `Idempotency-Key`: reserving again returns the donor's
  current `RESERVED` or `WAITLISTED` status unchanged, cancelling again returns
  `CANCELLED`, and scanning again returns `already_checked_in: true`. Cancelling with no
  reservation is `404`, and reserve or cancel once the event has started is `409`.

### Forward compatibility of enums

OpenAPI lists the values the server sends today, and the web relies on those types, so
the enums stay. A later minor version may add values, so a client MUST tolerate a value it
does not know rather than fail to decode the response. This covers `EventView.type`,
`EventView.reservation_status`, `ReservationResult.status`, `Subscription.status` and every
other enum in the bundle. Show an unknown event type as a generic event, an unknown
reservation status as no reservation action, and treat an unknown `Subscription.status`
as not active. Strict generated decoders (the Swift generator rejects an unlisted enum
string) need a tolerant wrapper or the enum stripped from the vendored schema.

`Subscription.status` does not gain `trialing` or `paused`. Monthly gifts start with no
trial and are never paused, and the server refuses to record a subscription whose Stripe
status is outside the documented six, so those values cannot reach a client.

## Compatible additions in version 2.6

Version 2.6 adds the one operation the apps lacked to start a monthly gift. `beginCheckout`
stays web-only: it is keyed by an email and returns to website pages.

- `POST /donor/me/recurring/checkout` (`startRecurringCheckout`, native only, `account`
  rate limit) takes `{amount_minor}`, validated like every gift amount (a whole number of
  dollars a day, $1 to $30, times 30), and an `Idempotency-Key`. It acts for the signed-in
  donor, so the body carries no email. `201` returns `{url, session_id, expires_at}`: open
  `url` in the system browser. `401` signed out, `403` not a native request, `409` a monthly
  gift is already active, the key was used with another amount, or another start for the
  donor is mid-flight (retry), `422` invalid amount or key, `429`, `503` (`PAYMENTS_UNCONFIGURED` when the
  deployment has no processor, or the earlier session could not be expired; retry).
  Retrying with the same key and amount returns the same session until it lapses at
  `expires_at`.
- Version 2.6.1 changes no schema, only this behaviour: from 2.6.1 a new start no longer
  answers `409` while the donor's own earlier checkout is open and unpaid.
- Closing the hosted page without paying never locks the donor out. Starting again, with
  any new key, while their earlier checkout is still open and unpaid returns that same
  session (same `url`, `session_id`, `expires_at`) when the amount matches, so show it
  again. A different amount expires the earlier session at Stripe and returns a new one.
  If the earlier session was paid just as the request arrived the answer is `409`: poll
  `GET /donor/me/recurring` for the active gift.
- The website's `beginCheckout` (web only, not bundled) now also lets its donors restart:
  it expires an earlier open session for the email and creates a new one. That includes a
  session the app opened, so an app checkout can be closed from the website; treat an
  expired session like an abandoned one and start again.
- Checkout returns to `<origin>/app/return?result=success&session_id=<id>` or
  `<origin>/app/return?result=cancel`. The origin is `DONOR_NATIVE_RETURN_ORIGIN`, else the
  public origin. Neither address proves payment: poll `GET /donor/me/recurring` until its
  `subscription` is present, because the gift becomes active only when Stripe's verified
  webhook arrives. `/app/return` is not in `links.json` or the web app's association files,
  so a browser shows a notice and the app must watch for the address (an
  `ASWebAuthenticationSession` callback matcher or a Custom Tab redirect). To open the app
  from it instead, add the path to the Apple `components` and the Android intent filter in
  the same release as the app change.
- `Subscription.status` documents its fixed set: `active`, `past_due`, `unpaid`,
  `incomplete`, `canceled`, `incomplete_expired`. Only the first three renew. Treat an
  unknown value as not active.

## Compatible additions in version 2.5

Version 2.5 documents statuses the API already served and gives shared inline response
bodies their own names. No operation, field or behaviour changes.

- Every authenticated read documents `429` (`RateLimitedByIngress`): the ingress rate
  limit can reject any request before it reaches the API, with no JSON body and possibly no
  `Retry-After`. The API itself applies no limiter to reads. Clients should back off and
  retry, and must not decode the body.
- `POST /donor/me/events/{event_id}/reserve` and `/cancel` document `403` and `429`, and
  `POST /donor/me/events/check-in` documents `403`; the origin and rate-limit guard
  already produced them.
- Named response schemas replace inline duplicates, so generators no longer name one
  operation's body after another: `SignupSent` (`postDonorSignup`), `SignInResult`
  (`finishDonorPasskeySignIn` and the shared `SignedIn` response used by
  `finishPhoneSignIn`, `oidcSignIn` and `postDonorPasswordSignIn`),
  `PartnerSignInLinkSent` (`partnerRequestSignIn`), `PartnerSaved`
  (`partnerSaveProfile`, `partnerSaveLogo`), `PartnerSignedOut` (`partnerSignOut`) and
  `InviteOnlyEvents` (`inviteOnlyEvents`). Wire bodies are unchanged. Generated type names
  change: the Kotlin `BeginPhoneSignIn200Response`, `FinishDonorPasskeySignIn200Response`,
  `Login202Response`, `EmployeeWorkforce200Response`, `LogoutAll200Response` and
  `DonorEvents200Response` no longer name those operations' bodies.

## Compatible additions in version 2.4

Version 2.4 adds one optional request pair and corrects two documented behaviours; no
operation is added or removed.

- `POST /donor/password/sign-in` accepts optional `device_name` (up to 64 characters) and
  `platform` (`IOS` or `ANDROID`) with the same rules as phone, OIDC, passkey and claim
  sign-in, so a password session can be labelled in the donor's session list. Browser
  requests ignore them. Signup issues no session; its emailed link is exchanged through
  `POST /donor/claim`, which already takes both fields.
- `POST /donor/signup` answers `200 {"sent":true}` (as the server always has), not the
  `202 {"message"}` the contract used to describe. Clients must accept any 2xx and ignore
  the body.
- `POST /donor/me/password` answers a missing or wrong `current_password` with
  `422` and `code` `CURRENT_PASSWORD_INCORRECT` instead of `401`, so a typo no longer looks
  like a lost session. `detail` is unchanged. A `401` still means the session is gone.
  Clients should treat a `401` with the same `detail` from an older server the same way,
  and must tolerate an unknown `code` in an error body.

## Compatible additions in version 2.3

Version 2.3 adds response fields only; no operation is added or removed. The partner
overview carries `user_id`, the calling partner user's own id, so a client can recognise
itself in the users list and withhold self-removal (the server also refuses it). The
donor dashboard carries `rankings_opted_out`. Clients must ignore response fields they
do not know.

## Compatible additions in version 2.2

Version 2.2 adds donor signup and password authentication, optional checkout signup
fields, badge artwork, and analytics event/journey UUIDs. Existing financial request
shapes remain accepted. Staff-only co-funding, spend publication and recognition
controls remain outside the native-admitted contract. Native clients should emit the
shared analytics taxonomy only after consent and use stable event UUIDs on retries.

## Compatible additions in version 2.1

Version 2.1 adds authenticated invite-only event calendar downloads, partner device
registration and push delivery, and application-status receipts. Existing version 2.0
request and response bodies remain unchanged. Partner notifications open `/partner`
and use the partner session, whose existing 12-hour lifetime also bounds registration.
Applications that expose pending application status must securely retain their separate
receipt; it does not authenticate a partner user. See `docs/operations.md` for lifecycle
and recovery behavior. The web donor sign-in page also offers the existing phone-code
API, alongside email and passkeys.

## Moving the apps from foundation 1 to 2

Version 2 contains the expanded R1 donor and partner API. Both native foundations
pin its manifest and payload hashes, generate their scoped session clients, and admit
`/account/gifts` for gift notifications. The bundle includes public initiative detail/spend
and event detail/calendar reads for the next product screens.

Version 1.0.0 at `5e237101c0ad81aa4675d16a94eb64863c5276b9` predates the identity,
volunteer, ranking, and notification fields now returned by the profile endpoint. Its
Swift decoder rejects those additional fields. Updating the foundation fixes new builds;
it cannot repair an installed version 1 decoder. If a version 1 app has been distributed
against a real environment, preserve its response contract through an explicitly
versioned API during migration. Future response additions also need installed-client
compatibility tests before deployment.

## Swift generator compatibility

The compile probe in `contracts/mobile/swift` pins Apple's generator 1.14.1, runtime
1.13.1, URLSession transport 1.3.2, and transitive dependencies in `Package.resolved`.
It generates and compiles the bundled Swift input and tests session/profile decoding,
unknown and populated financial states, recurring subscriptions, event reservations,
and literal-null completed payment URLs.

The canonical and mobile contracts preserve their original schemas. Apple's pinned
generator skips standalone null branches, so `swift.openapi.json` expresses nullable
object/scalar unions as nullable types. Referenced object fields and constraints are
retained; nullable enums explicitly include null. A null-only payment URL uses the
`DADLiteralNull: DADNullValue` type override from the probe's configuration. Its supported
schema placeholder is only a generator input: the custom Codable type accepts and emits
literal null and rejects every non-null value. Apps adding payment-update operations must
include that override and the bundled source; DAD-iOS's generator configuration carries it.

The probe no longer skips nullable fields. Unused public-import warnings emitted by the
pinned generator with Swift 6.4 remain visible; no warning is suppressed.

## Real native client acceptance

With both native repos alongside this checkout, the normal native tooling installed,
and a migrated synthetic database, run:

```sh
TEST_DATABASE_URL=postgresql://go_test:go_test@localhost:55440/donor_go make test-native-clients
```

`DAD_IOS_DIR`, `DAD_ANDROID_DIR`, and `DAD_SWIFT_BUILD` override the checkout/build paths;
the defaults are `../dad-ios` and `../dad-android`, so set them when the checkouts are
named `DAD-iOS` and `DAD-Android` on a case-sensitive file system.
The command runs the backend's database-backed session lifecycle test, then holds open
its test-only HTTPS server on loopback for the actual Swift URLSession and Kotlin Retrofit
clients. Private temporary fixtures contain separate one-use proofs and the test server's
certificate. The native test transports trust only that certificate; production trust
configuration remains unchanged. The server exists only in the Go test binary, inherits
the synthetic-database guard, uses the least-privilege API role, and calls no providers.

Each client proves claim and profile decoding, single-use claim rejection, locally
expired access triggering refresh and atomic replacement, rejection of replaced access,
remote revocation clearing local credentials, and logout revoking both credentials.
The opt-in Swift build flag and Gradle property compile these integration suites only
for this command; ordinary unit tests retain their existing scope without skipped tests.
The command stops the server and removes credential fixtures on completion or failure.
Cross-repo CI needs authenticated access to all three pinned revisions before this can
be a required hosted check. This local acceptance does not establish provider delivery,
OS app-link association, or physical-device acceptance.

## Design source

Edit `design/tokens.json`, then run `make generate-mobile`. The web adapter consumes its
generated TypeScript representation. `make check-mobile` fails on drift. Native lengths
are reference values for points/dp; text adapters must support Dynamic Type/font scaling,
and platform font families remain platform decisions. Colors retain their original
hex/RGBA strings so adapters can handle opacity consistently. This foundation does not
establish approved native layouts or visual/accessibility acceptance.

## App foundation boundary

The mobile implementation decision is separate SwiftUI/iOS and Kotlin/Compose/Android
applications in `DAD-iOS` and `DAD-Android`, approved on 18 October 2026. This supersedes
the historical PRD's single cross-platform codebase requirement. DAD-Main continues to
own business rules, API contracts, design values, and server integrations; client code
owns native presentation, lifecycle, secure credential storage, and platform interaction.
The R1 PRD defines the required donor and partner features; the native foundations
are the transport and session starting point for those screens.

Both apps implement the session lifecycle: authenticated reads, single-flight
refresh and atomic token replacement, cancellation/timeouts, revoked-session handling,
and local credential cleanup on logout. The automated tests use synthetic transports, and
the acceptance command below proves the same behavior against the real API; device
acceptance remains open. A refresh response arriving after logout must never restore the
session. Secure persistence must avoid logs and backups of credentials;
Android uses encryption keys protected by Keystore, iOS uses Keychain.

Link handling accepts only the configured HTTPS public origin and approved paths.
Proof tokens stay in memory and out of logs. Signed-out destinations wait for account
access; referral and company-code links are captured natively, and unsupported product
destinations retain their web behavior. Push
payloads select only approved relative destinations, never arbitrary URLs. OS association
files require the final app identifiers and signing certificates, and a reachable direct
API ingress is required for a real-device integration test. Neither is fabricated here.

Production signing, Apple and Google sign-in configuration, analytics activation, and
real-device/provider acceptance remain open; each app's README lists what is wired against
the API and what is switched off until configured. The R1 PRD defines donor and partner
scope. Offline financial mutations require a separate design rather than replaying commands
blindly.

## Partner sessions in the apps

The partner portal's operations are in the bundle and accept the app's bearer token.
Native requests send `X-DAD-Client: native`, `Authorization: Bearer <session>` and no
`Origin` or cookies; a request with a cookie or `Origin` is judged as a browser request
by its cookie alone. The web proxy's dropping of `Authorization` does not affect the
apps, which call the API directly.

1. `POST /donor/partner/sign-in` with `{"email": "..."}` answers 202 whether or not the
   address belongs to a partner user, and emails a `/partner/accept#token=...` link.
   Invitations and approval emails carry the same link.
2. `POST /donor/partner/accept-invite` with `{"invite": "<link token>"}` answers
   `{"session": "<token>"}` to the app; a browser receives the HttpOnly
   `__Host-dad_partner_session` cookie (`dad_partner_session` over HTTP) and
   `{"signed_in": true}` instead. The link works once.
3. Every partner read and write takes the session as a bearer token. Mutations are
   rate-limited per account as on the web, sign-in and link exchange at the sign-in
   limit, and admin-only actions check the partner user's current role. `GET /donor/partner/application-status` instead
   takes the applicant's private application receipt as its bearer token; it is not a
   partner session.
4. `POST /donor/partner/sign-out` revokes the session and clears any push registration.

A partner session lasts 12 hours, on the web and in the app, and has no refresh token:
when it ends (401 `Partner sign-in required`), the app asks for a new emailed link. The
partner session is not created with a device name or platform; partner push registration
records the platform.

A device can hold a donor session and a partner session at once. They are separate
tokens in separate stores: a donor token never authenticates a partner route and a
partner token never authenticates a donor route, and signing out of one leaves the other
signed in. Apps keep the two in separate secure-storage entries and send the partner
token only to `/donor/partner/` routes.
