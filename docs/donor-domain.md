# Donor domain

This repository contains the R1 donor, partner, and staff products. Donor, partner, and
staff identities are separate. Recipient, aid-delivery, AI, chat, and blockchain
functionality is outside this release. The repository remains MIT licensed.

The following R1 rules were agreed with the product owner. They are the implementation
contract, not a claim that every capability is already shipped.

## Giving and identity

Stripe processes USD donations through hosted checkout. Giving is recurring only: a
whole number of dollars a day, from $1 to $30, billed monthly as the daily amount × 30
($30 to $900 a month, G-1, G-2). The donor chooses $1, $3, or $5 a day or types another
daily amount; larger gifts are arranged with DAD directly. The daily amount and the
monthly charge always appear at equal visual weight; an amount set before this rule
shows its daily figure as "about". DAD absorbs fees; there is no cover-fees prompt.

Giving collects a private name, email, ZIP and city. A verified account is not required for hosted checkout. Matching is case-insensitive and does not
remove dots or plus tags. Different addresses are separate identities until an explicit
merge verifies both. Never infer identity from a shared card, device, or postal address.
The initial implementation accepts ASCII mailbox names and IDNA domains.

One email holds one role (PRD §3). Giving, a donor email change, a staff invitation, a
partner invitation, and partner approval each refuse an address that already signs in as
a donor, an active staff member, or an active login of another partner organization. A
partner employee who gives uses a different email. Addresses that held two roles before
this check keep working; operators can list them with the query in
[operations](operations.md#one-email-one-role).

A company-code visit is captured before giving and kept for 30 days. It links the donor
to the employer when they prove account ownership in that browser, or when they enter
the code in their signed-in profile. Starting a guest checkout with someone's email
cannot change their employer or attribute their giving to a company. An email link
opened in another browser needs the code entered after sign-in; app-install attribution
still requires a deferred-link integration.

Email claim and sign-in links last 15 minutes and are single-use. A browser session lasts
at most 30 days; an app session lasts while the app refreshes it at least every 60 days,
and at most a year. Donors can see where they are signed in and sign out of any other
session. Credentials are stored hashed; the email delivery queue temporarily holds an encrypted
copy of the login token until delivery or expiry. Changing email requires fresh
authentication through the existing address and verification of the new address.
Knowing an email or checkout ID never grants access to giving history.

One active monthly subscription per donor; one-time gifts and donor-initiated pauses are
not offered in R1.
Amount changes apply at the next renewal without proration. Cancellation stops future
renewals without refunding past gifts and plainly warns that the streak will end. Donors
see their monthly gift as last
synced from Stripe, with the time of that sync; confirming a change checks Stripe's
current state and refuses the change if it no longer matches what the donor confirmed,
saving that state so the donor sees it on refreshing.

## Referrals

The most recent valid referral touch before the first gift wins, for both referrer and
channel (LINK or IRL). A later scan or link replaces the prior touch and starts its own
30-day window. Checkout stores attribution
before account creation, and each collected gift records the referrer and channel that
were valid at the moment of charge. Conversion uses the earliest verified successful
payment's collection timestamp, with the Stripe payment object ID in ascending lexical
order breaking timestamp ties. Starting checkout is not conversion. That first payment
must carry a valid referral within its window; a direct first payment earns no referral
conversion. Later gifts add dollars, never additional conversions.

Webhook arrival order does not decide credit. If an earlier payment is verified later,
the Go projection corrects the first gift, referrer, and channel and transfers the donor's
realized net dollars and single qualifying conversion atomically between the affected
totals. Original gift facts remain unchanged. Each change to an established first gift
is appended to the staff `referral-corrections` export, including old/new payment IDs,
referrers, channels, recording time, and rules version. Recomputing unchanged facts adds
no correction. A previously earned Founding Donor badge remains permanent.

Pending referral notices are checked against current attribution before delivery.
Corrected recipients can receive their own notice, once per referred donor, recipient,
and channel; existing legacy notices prevent duplicate sends. Already delivered notices
cannot be recalled. The correction history records the attribution change without
inventing a second financial gift or silently changing historical notification records.

A referral touch that led to a gift carrying its referrer is kept for good. Any other
touch, including one whose checkout was abandoned or paid after the window, is deleted 13
months after capture, long after its window closed, so retention never changes
attribution: checkouts and gifts keep their own copy of it.

Self-referral credit is removed when giver and referrer match by account or normalized
donor email, or when they share a card or an app installation; the gift itself remains
allowed. A card is matched by Stripe's card fingerprint, read once per payment method
from the subscription it pays; an installation by a SHA-256 of the push token it
registered. Either signal recorded after a gift was credited brings both donors'
projections due, so the credit is withdrawn at once. The signals are opaque and are kept
when an account is deleted, so deleting a second account cannot restore its credit.
There is no second-level credit.

Sharing requires a claimed account and an explicitly chosen public first name/nickname.
Do not substitute a billing name or email. Codes are permanent by default and may be
replaced, invalidating the old code for new visits. Existing captured attribution remains
a record of the original valid visit. Screenshots carrying the IRL marker still classify
as IRL: this indicates entry method, not verified physical presence. There is no print
or image-download feature. Link and IRL counts appear equally; no combined count in R1.

## Refunds and recognition

Attributed dollars are collected donations after successful refunds and before processing
fees. Pending refunds are separate; reconcile a later refund failure. Retain original
gifts and record adjustments instead of deleting financial history.

A referral counts while the referred donor has any unrefunded successful gift amount.
Fully refunding all gifts removes the count. A later gift restores the same relationship,
not a second conversion. Historical conversion events remain distinguishable from the
current count. Refund and subscription cancellation are separate staff actions.

A giving streak grows by one local calendar day while the monthly recurring gift is
current. Billing-period continuity is retained internally so renewals, refunds, and late
processor facts can be reconciled without inventing days. A failed renewal gets 7 days
from its scheduled date: the day streak remains current and at risk during grace, then
resets after the deadline if payment has not recovered. A renewal not yet reported by
the processor keeps the current state until the deadline; after that the streak is
unknown until facts arrive. A cancellation ends continuity at the first skipped renewal,
not immediately. Amount changes do not break continuity. Partial refunds retain
the period if recurring funds remain; full refunds disqualify it unless another qualifying
recurring payment covers it. Recalculate subsequent continuity rather than erasing every
later month. Staff repairs recompute recorded facts. Super administrators may separately reset the current streak with an audited reason; resets survive recomputation and preserve financial history and earned badges.

A streak milestone is reached at 7, 30, 100, 365, 500, 1,000, 1,095, and 1,460 days,
then every further 365 days. Each milestone is congratulated once per streak run, which
is identified by its first current day; a streak that resets and is rebuilt is a new run.

Content staff may change the words on a badge (its title and how-to-earn text) and on each
named streak milestone's notification; the yearly milestones after 1,460 days use the
standard wording. Numeric earning thresholds and badge artwork are editable through content administration; signal types stay fixed, and every change is audited.

Top 2% and Top 50 City are awarded only when the rank was taken among at least 100 ranked
donors (nationally, or in that city): in a small population every donor would qualify.
Content staff can change that minimum with the badge's other earning rules. A donor's own
rank line is shown whatever the population.

Any successful first gift before 2027-01-01 00:00 America/New_York earns Founding Donor
status. Use the processor's successful-payment timestamp, not checkout start or webhook
arrival. Website-waitlist sign-ups and earlier website donors (D-6) are Founding Donors too:
staff import their addresses with sign-up dates before the cutoff, and a donor whose email
matches earns the badge at their next update, without needing a gift before the cutoff. Only
a keyed hash of each imported address is kept, and qualifying claims do not expire. Claiming after cutoff is allowed. The badge is
permanent, including after refunds/cancellation. Changing the cutoff requires a product
decision; existing awards remain permanent.

A company team wins a month (Winning Team) when, at the close of the calendar month in
America/New_York, it has the most combined active giving among its company's teams:
members' current monthly gifts, annualized, using the displayed leaderboard's ordering. Teams below the minimum group size are not ranked, a month in which no team
gave has no winner, and the standings are recorded once. Members of the winning team when the
month closes earn the badge.

Team Captains reserve event places 48 hours early (B-3). Staff may set when an event's
booking opens; until then only a Team Captain can reserve, from 48 hours before it. An
event with no opening time opens to everyone when it is published.

## Staff and messages

A super administrator holds every staff power: donors and money (search, refunds,
cancellations, resent messages, exports), badge grants, content publishing, the audit
log, and staff access. Partnerships operators manage partner applications and partners,
Growth operators manage events, and Impact operators manage initiatives; every role reads
the report. Require individual staff accounts and passkeys. Enforce
named permissions server-side; audit actor, time, target, and result, with reasons for
refunds and staff cancellation. No donor impersonation or access to card details.

Send receipts per successful gift and a prior-year summary showing gifts/refund
adjustments. Send failed-payment recovery at failure and on grace day 5, before the
7-day deadline; stop on resolution or cancellation. Optional referral-converted emails
identify the channel without the referred donor's name or amount. Send the initial
account-claim email with replacement-link access, without promotional reminders.
The donor app's notifications follow the same facts: a referral converted (with its
channel, not the referred donor's name or amount), a failed renewal putting a streak at
risk (once per failed period), a streak milestone, a streak that ended after grace, an
earned badge, a waitlist promotion, event reminders and nearby events, and an event
cancellation. Each except the cancellation is optional and starts on once the app is
allowed to notify, since the device's permission prompt is the opt-in; preferences are
rechecked before sending. [Messaging](messaging.md) lists every message and notification.
Marketing consent is separate. Receipts/security/essential payment notices are distinct
from optional messages, subject to approved legal classification. Prepare the enrolment
template at R1 and activate it with R2 enrolment. Finance approves summary treatment;
legal approves receipt wording.

The receiving entity's US nonprofit status, EIN, and existing Stripe account remain
unconfirmed. They must be verified before live donations.

## Implementation and release boundaries

Go implements the donor API, Stripe inbox, recurring controls, projections, account
email, notification outbox, staff passkeys and permissions, audited refunds/cancellation,
exports, private source documents, spend by category, partner applications, and
the partner portal.
TypeScript implements the public, donor, and staff web surfaces. See [architecture](architecture.md)
and [operations](operations.md) for runtime behavior and instructions.

The receiving entity remains unverified and the application accepts test payments only.
Gift records and summaries are explicitly sandbox records, not approved tax receipts.
Terms, Privacy, and donor consent need approved content. [Wallet issuance](wallet.md) is
implemented behind optional issuer configuration; real-provider/device acceptance remains
open. [Analytics](analytics.md) defines available financial facts and consented first-party browser
collection. App design deliverables and operational release acceptance remain
separate from the backend rewrite. The web referral-code page
offers full screen, keeps the display awake while shown, and gives manual brightness
instructions; it cannot promise hardware brightness control. Email changes require single-use confirmations at both the current
and proposed address, each expiring after 15 minutes. Completing a change preserves the
donor ID, revokes existing sessions and sign-in links, and records identity audit events.
An address already attached to another donor is rejected after verification; accounts
are never merged automatically. Historical checkout and processor billing records retain
their original details.

The inactive enrolment queue and header-only export exist at R1. There is no enrolment
trigger until R2; the [prepared confirmation wording](messaging.md) does not promise funds.
