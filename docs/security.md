# R1 security controls

Donors authenticate through single-use, 15-minute email proof, a passkey they added
while signed in, a password set after account verification, a one-time SMS code,
or Apple or Google sign-in, and hold hashed,
revocable sessions: a 30-day cookie in browsers, a rotating token pair in the app. Staff sign in with individual passkeys (WebAuthn, verified by
go-webauthn) and receive eight-hour hashed sessions. Donor and staff cookies are
separate, HttpOnly, and Secure on HTTPS. Staff cookies use SameSite=Strict; donor cookies
use Lax. Over HTTPS the donor and partner session cookies carry the `__Host-` prefix
(`__Host-dad_donor_session`, `__Host-dad_partner_session`), so browsers refuse one with a
Domain or a Path other than `/`, and a sibling subdomain cannot plant one. Every sign-in
and sign-out expires the unprefixed name as well, so a browser never holds two live
sessions. Sessions issued before the prefix are still read from the unprefixed name, below
the prefixed one, until they lapse (30 days for donors, 12 hours for partners); that
fallback in `sessionCookieValue` can be deleted once the longer window has passed. Over
plain HTTP, in development, the names are unprefixed. Native clients use bearer tokens and
are unaffected. Anonymous login responses do not reveal whether an address exists, and a donor receives at
most one sign-in link a minute and five an hour however many addresses ask.

The back office runs on its own host, `DONOR_STAFF_ORIGIN`, which the API requires to
be neither the public host nor a parent domain of it, since a site may use passkeys for
its parent domain. Both origins must be written exactly as browsers send them, because
Origin headers and passkey client data are compared with them byte for byte. The web server answers only the back office and its API
there, marked noindex, and none of it on the public host. The staff cookie is host-only
(`__Host-dad_staff_session` over HTTPS), so it never reaches the public site, and a page
on either host cannot pass the other's Origin check. The staff host is the passkey
relying party: passkeys are bound to it and are useless to any other site. The two
hosts are the same site, so SameSite=Strict alone would still send the staff cookie with
a link followed from the public host; staff routes therefore also refuse any request whose
`Sec-Fetch-Site` is present and neither `same-origin` nor `none`, which covers reads with
effects such as audited export downloads. The web proxy forwards that header to the API.

Staff sign-in is usernameless. The browser offers a discoverable passkey for the staff
host, so there is no email field, no account enumeration, and no password to guess or
lock out. Every challenge is random, held server-side in a single-use ceremony that
expires after five minutes, and deleted when answered. The API verifies the challenge,
origin, relying party, user verification (required), signature, and signature counter.
Synced passkeys always report a counter of zero, which is accepted; any other counter
that fails to advance is treated as a cloned passkey and refused. A disabled account or an
unknown passkey cannot sign in, and an unknown passkey still spends its challenge. Failed
sign-ins with a known passkey are audited as `staff.sign_in_failed` with the reason:
`invalid`, `counter`, or `disabled`. Invitations expire after 24 hours, allow five
attempts, and register a passkey bound to that account; a rejected attestation or a
passkey already registered to an account counts as an attempt and is audited as
`staff.activation_failed` (`invalid` or `credential_in_use`). Only the transports WebAuthn
defines are stored with a passkey. Recovery deletes every passkey of the account, revokes its sessions and
invitations, and issues a new invitation. Attestation is not requested, so the passkey
provider (device, platform, or password manager) is not verified; passkeys are
stored as public keys and need no encryption key.

Donor passkeys follow the same ceremony rules on the public host, which is their relying
party; a public origin addressed by IP cannot be one, so such a deployment runs without
them, and its passkey routes answer 503 `PASSKEYS_UNCONFIGURED`. Sign-in is usernameless and requires a discoverable, user-verifying passkey; any
answer spends its challenge, refused ones included, and a disabled donor or unknown passkey
cannot sign in. A passkey sign-in is a session like a claim, recorded in
`donor_session_events`. Only a signed-in donor can add a passkey: the creation options
exclude the passkeys they already hold, the answer must come from the session that began
the ceremony, and a passkey registered to any account is refused. Adding one emails the
donor an essential notice that removing the passkey cannot withdraw. Donors list and
remove their passkeys on the profile, where an ID that is not theirs is not found.
Completing an email change removes every passkey with every session, because the email is
the account's root credential; signing out everywhere keeps them.

Phone sign-in sends a six-digit code to a phone already saved on an account. The code is
stored hashed, expires after 10 minutes, and allows five attempts. A wrong guess commits
while holding the code lock, before the request is rejected, so concurrent guesses cannot
exceed the budget. A replacement is limited to one a minute per phone across API replicas;
an immediate retry preserves the existing code and attempt count. Each phone also has
20 wrong guesses across all its codes in a rolling day; once they are spent, no new code
is sent and none is accepted until the day has passed. Changing or removing a
phone, or deleting its account, invalidates its pending code. Requesting a code answers the same whether or not the number has an account, and
a wrong, expired, or exhausted code and an unknown number all fail with one message.
Apple and Google sign-in verify the ID token's RS256 signature against the provider's
published keys, then its issuer, audience, expiry, and issue time, then sign in the donor
already linked to that subject or, on first use, the donor with the token's email,
recording the link. The authorized presenter must be a configured client, and a token
with multiple audiences must name one. The email is used only when the provider marks it
verified. Google must also be authoritative for the address: Gmail or a token with a
Workspace hosted-domain claim. Google can mark an external mailbox verified without
proving its current ownership, so that address cannot create a link; use email sign-in
instead. An established provider-subject link still works without the email claim.
A first-time verified provider identity creates a donor account if the address
does not hold another role. Donors may also create accounts through email-verified
signup before giving. Both SMS and provider sign-in stay off until
their provider is configured ([operations](operations.md#phone-and-social-sign-in)); the
recording stubs for development must never be configured in production.

Partner portal users are separate from donors and staff and cannot give. They sign in
through emailed single-use links: 15 minutes for a link they request, seven days for an
approval or invitation. A user holds one live link at a time, and a link replaced before
delivery is never sent. Requesting a link never reveals whether the address belongs to a
partner. Sessions last 12 hours, in the `__Host-dad_partner_session` cookie (`dad_partner_session` over HTTP) or as a bearer token
in the app, and removing a user revokes their sessions. Administrators alone invite and
remove users, edit the organization profile, and export reports.
Redemption locks the invitation so concurrent exchanges create one session. Removing a
user also invalidates their unused link; a later invitation cannot revive it. Partner
administration and staff changes to partner users serialize on the organization, then
recheck the caller's access before writing. An administrator cannot demote themselves by
reinviting their own address as a viewer. Approval and rejection lock the application,
so competing staff decisions cannot create duplicate organizations.

The donor app uses neither cookies nor Origin. It sends `X-DAD-Client: native` on
every request and authenticates with `Authorization: Bearer <access token>`. A request
is native only when it has that header and neither an `Origin` nor a `Cookie` header;
any other request is a browser request, whose Authorization header is ignored and whose
mutations need the exact Origin. Browsers cannot add a custom header to a cross-origin
request without a CORS preflight, and the API grants none, so another site cannot forge
a native request.

`POST /donor/claim` returns tokens in the body only to a native request and then sets no
cookie: a 15-minute access token and a refresh token, `<family>.<secret>`. Each part is
256 random bits, and only SHA-256 hashes are stored, so lookups by hash leave no secret
to compare in variable time. The family part is constant for the session and finds it;
it reveals nothing about the access token or the session ID. `POST /donor/refresh`
refuses every browser request, even from the public origin, and exchanges the refresh
token for a new pair, retiring the previous access token at once. A refresh token works
once: presenting one its session has already exchanged revokes the whole session and
records `REFRESH_REUSED`, because either the app or a copy of the token is stale. The one
exception is the token exchanged most recently, accepted again for one minute after the
exchange and answered with a pair that supersedes the first, so a response lost on a
mobile connection does not sign the donor out; a stolen token used within that minute
still exposes itself when the app next refreshes with the superseded pair. Concurrent
refreshes are serialized by a lock on the session row, and the later answer wins. Each
refresh extends the session to 60 days after it, never past a year after sign-in. Access
and refresh tokens are looked up in different columns, so neither is accepted as the
other.

Every request checks its session in the database, so sign-out, signing out another
session or every other session, sign-out everywhere, refresh-token reuse, and email
changes end a session at once; the access lifetime only bounds the use of a token that
leaks. Donors see each session at `GET /donor/me/sessions`, as a browser, which is not
fingerprinted, or the app with the device name it reports (stripped of control and
formatting characters and capped at 64 characters) and platform, with sign-in and last
use recorded at most every five minutes. They end one by its random session ID, which is
unrelated to any token, and an ID that is not theirs is not found. Sign-ins and
sign-outs of other sessions are recorded in `donor_session_events`. Native requests are
rate limited in the same classes, refreshes in the account class rather than the
sign-in budget. Only donor account routes admit the app (sign-in, claim, passkey sign-in,
refresh, email-change confirmation, sign-out, `/donor/me/*`) and referral and company-code
link capture, which hands the app the visitor token a browser keeps as an HttpOnly cookie;
web checkout, partner signup, and all staff routes stay browser-only, and staff sessions are
never accepted as bearer tokens. The app must keep its tokens in the platform keychain/keystore and send
one refresh at a time. The Next proxy does not forward `Authorization` or
`X-DAD-Client`, so the app calls the API host directly. The bundled nginx serves only the
web hosts; an API host needs its own ingress, listed in `DONOR_TRUSTED_PROXY_CIDRS`, that
overwrites `X-Real-IP`.

Push tokens are stored on the app session that registered them and serve only to
address notifications; they are not credentials. Every revocation clears the session's
token, which a database check constraint requires of any revocation, and delivery
skips sessions that have lapsed, so a signed-out or expired installation receives
nothing more. Registration is app-only, validates the token's form for its platform,
and moves a token held by another session, since a token names one installation. A
SHA-256 of each registered token is kept against the donor to block self-referral
credit ([donor domain](donor-domain.md#referrals)); it cannot address a notification.
Notification text names no one and no amount: it transits Apple's and Google's servers
and shows on lock screens, so the app fetches detail through the API, which shows a
referral conversion only as a time and a channel. A token is dropped only when the
provider says it is invalid; a refused configuration parks the notification instead,
so a wrong key or topic cannot erase every registration. The APNs `.p8` key and the
Firebase service-account key are read from private files at startup, sign provider
tokens locally, and never leave the process or reach logs; provider responses, which
can echo tokens, are not logged.

Each process connects to PostgreSQL with its own login, a member of one role that holds
only the privileges its statements need ([database roles](operations.md#database-roles)):
no process can change the schema, the monitor reads only queue status, `rekey` only the
encrypted columns, and workers that merely lock a donor cannot change one. A process refuses
to start as the schema owner or a superuser, or without its role. Credentials reach only
their own container, and migrations restore the declared privileges on every run, so a
manual grant does not persist. Provisioning sends the database only a SCRAM verifier of
each password. Group roles span the PostgreSQL cluster, so each environment needs its own.

Named permissions are checked server-side against current staff state. Ownership comes
from the session and database, never an email or customer ID supplied by the browser.
Financial mutations record their actor, target, reason, immutable command, and result.
Staff recovery/role changes revoke sessions and preserve the last active administrator.

Browser mutations require the exact Origin of their surface: the public origin for donor
and public routes, the staff origin for staff routes. Requests, pages, uploads, passkey
responses, exports, and provider calls have bounded sizes or timeouts. The Next proxy
exposes `/donor/*`, `POST /analytics/events`, and the exact public event-detail, calendar,
and initiative-spend GET/HEAD paths. Everything else is refused before contacting the API.

Browsers reach the web service only through nginx ([edge](edge.md)). It accepts TLS 1.2
and 1.3 for the configured public and staff hosts only, serves back-office paths only on
the staff host and donor paths only on the public host, and marks the staff host
`noindex`. It sets `X-Real-IP` to the client address on every request and drops other
client-chosen address headers; behind Cloudflare it takes the address from
`CF-Connecting-IP` only from Cloudflare's published ranges and serves only connections
with an Authenticated Origin Pulls certificate. Rate limits are layered: Cloudflare's
distributed rules, nginx's per-client limits, and the API's per-instance buckets
([edge](edge.md#rate-limits)). nginx's access log records the time, client address, host,
method, path, status, response bytes, durations, and Cloudflare's `CF-Ray`, never query
strings, cookies, or referral codes. Client addresses are personal data, so keep logs no
longer than the approved retention policy allows ([operations](operations.md#database-and-containers)).
nginx's error log can include a request line, so treat it as sensitive.

Stripe handles all card entry. Signed webhooks are verified before persistence; only
allowlisted event fields are stored. Retryable commands are recorded before provider
calls, and no donor lock is held during the call. Current remote state is reconciled rather than trusting webhook arrival order.
Reads never call Stripe, so page views cannot multiply provider requests.
Logs must exclude donor payloads, credentials, tokens, and remote response bodies; API
failures are logged with route, status, and error type only. Every API response carries
`nosniff`, `no-referrer`, and a `default-src 'none'; frame-ancestors 'none'` content
security policy (file downloads use `default-src 'none'; sandbox` instead), and is
`no-store` except successful answers from the public event
detail, event calendar and initiative detail routes, which allow 60 seconds of caching.

Public content never includes donor data or private source files. Source documents use a
dedicated encryption key and SHA-256 verification. CSV exports use field allowlists,
permission checks, private cache headers, formula neutralization, and audit trails.
Staff exports are built in the background and stored as 24-hour gzip files in Postgres; only
the requesting staff member can download one, and only while their role permits the dataset.
A download is a sandboxed, `no-store`, `nosniff` attachment and is audited, as are the request
and the build's end.
Do not publish exports or treat a source document's existence as proof of its accuracy.
Referral touches store only a hash of the visitor token; touches that led to no
attributed gift are deleted 13 months after capture.

Queued sign-in links and email-change proofs are encrypted with the email key, and source
documents with the content key, as Fernet tokens (AES-128-CBC with HMAC-SHA256). Each
setting is an ordered key list: the first key encrypts and any listed key decrypts, so a
key is rotated without downtime by putting a new key first, re-encrypting stored tokens
with `rekey`, and then removing the old key ([operations](operations.md#encryption-keys)).
Every key in a list is validated at startup, and errors name the variable and position,
never the key. A retired key still opens backups taken before its rotation, so it is kept
in the secret store only as long as those backups and destroyed with them. Session and
proof lookups use unkeyed SHA-256 of random secrets. Website-waitlist claims instead use
a keyed hash of the normalized email address, with a separate Founding Donor claim key.

The frontend restricts connections to its own origin, denies framing, uses no-referrer,
and disables camera, microphone, and geolocation. Production CSP excludes unsafe-eval.
Account, partner-portal, and staff pages render per request with a nonce-based `'strict-dynamic'` script
policy set in `proxy.ts`; public pages keep a static policy that permits inline scripts
so they can be cached. Each response carries exactly one policy. Public links into donor
accounts and the partner portal use full-document navigation to install the nonce policy; staff entry is a
direct document load on the staff host. Any new public link into account routes must use
a native anchor rather than client routing. Both origins are compiled into the web build,
so its host routing and headers always agree. User content must never be inserted as
raw HTML. Email proof tokens use fragments removed before network requests. Donor and
partner email proofs require an explicit confirmation before exchange; visiting a link
alone does not consume it or replace the browser's account.

## Dependency advisory boundary

`GO-2026-5932` concerns the unmaintained `golang.org/x/crypto/openpgp` packages. The
module is pinned to a pseudo-version, after those packages were removed, that no tagged
release carries yet; move to the first tagged release that does when one exists. The
application and its tests do not import OpenPGP, and `make lint-go` fails if any package
in the application or test dependency graph does, then runs the unmodified vulnerability
scanner. Other `x/crypto` packages, including OCSP used by WebAuthn's certificate checks
and bcrypt, remain required, which is why `x/crypto` is a direct requirement.

Release still needs verified legal/entity details, approved retention and incident
procedures, managed secret storage, backup/restore evidence, the Cloudflare edge
configuration and origin firewall,
provider acceptance, and security/accessibility review. This document describes controls
implemented in code, not a claim of independent certification or production readiness.

## Checkout identity

Browser and native checkout both require a verified donor session. The browser's
email must match that account and is shown read-only. The checkout transaction
authenticates and locks the session owner, then rechecks the session after the
lock before resolving email, recurring status, referral attribution or an existing
checkout. Anonymous, expired, revoked, disabled and unverified accounts cannot
reach checkout; a signed-in donor cannot probe another address's role or giving.
Sign-in email links allow the exact `/donate` return path. Signup also answers
uniformly for unavailable staff/partner and disabled addresses, without creating
an account, so it cannot expose those roles as an alternative to checkout.
