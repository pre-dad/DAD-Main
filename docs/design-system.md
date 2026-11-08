# R1 design foundations

The live web palette, corner radii, spacing scale (`space`, 4–64 px), and type scale
(`text`, 12–36 px) are authored in `design/tokens.json`, a platform-neutral source.
`make generate-mobile` generates `apps/web/design/tokens.ts`; the versioned
[mobile foundation](mobile-foundation.md) also supplies Swift and Kotlin values.
The web adapter maps those values to CSS custom properties on the
server-rendered document; `globals.css` consumes them and holds no other palette colors
(only a gradient mask and the print rules use literal black, white and grey). The SwiftUI
and Kotlin/Compose applications adapt the generated values to native colors and sizing.
Typography loading, responsive layout, focus treatment, and browser motion
preferences remain web concerns in the stylesheet and root layout.

The donor dashboard opens with "Welcome back." (no name), one sub-line naming the next step
(a failed payment, a first invite, or a milestone 7 or fewer days away), and Invite someone
plus Show my code; the at-risk callout owns the one `.button`, so Invite someone becomes
secondary while it shows. A compact totals strip (giving, initiatives, events) sits above
Link and IRL counts, which stay side by side at equal visual weight, with realized dollars,
streak, and founding recognition in a row beneath them. A referral count of zero (never an
unknown one, which reads "Updating") shows as muted text with a guidance line. The Founding
Donor card is the one dark card and appears only once earned; otherwise a single line explains
how it is earned, with the days left computed in the browser. Account links sit in three
labelled groups (Your giving, Your circle, You) in the `aria-label="Account"` nav. The
loading skeleton reuses the loaded markup and must keep the nav within 10px of where it lands.
Responsive grid items must not inherit vertical document-flow spacing.
Buttons follow one hierarchy: `.button` is the primary brand action (one per view, meaning one inside `<main>`; the
header's Give button is excluded),
`.secondary` is the outlined default, `.quiet` is a text-weight action, and
`.secondary.danger` confirms destructive steps. Amount presets are outlined until
selected, and show the monthly amount and daily framing at equal weight. Browser tests cover desktop,
mobile, and tablet layouts, automated WCAG 2.2 checks, keyboard confirmation, and partial
or failed data recovery. Automated checks do not establish full assistive-technology
conformance.

Manual assistive-technology acceptance is deferred by product agreement, alongside
browser analytics. The future pass covers VoiceOver with iPhone/Safari, TalkBack with
Android/Chrome, and desktop keyboard-only navigation through giving, account claim and
sign-in, dashboard, preferences, recurring-gift changes, and critical staff tasks. Check
control names and reading order, focus movement and restoration, dialogs, validation,
loading/error announcements, and completion of each journey. Record device/browser
versions and results, fix failures, and retest; automated accessibility results alone
do not complete this acceptance.

These shared values are an engineering foundation, not completion of the agency design
deliverables. The R1 PRD's 24 donor and 11 partner screens, native component implementations, interactive
prototype, responsive/redline specifications, approved Wallet artwork, store assets, and
manual accessibility conformance still require design and device acceptance. The R2 app
remains unbuilt; importing these tokens must not introduce recipient or partner surfaces.

## Preview notice and launch constants

`PreviewBanner` (`components/donor/PreviewBanner.tsx`) is the single place the donor site
says it is a test environment. `SiteFrame` renders it above the sticky header, so it shows
on donor pages, the partner layout, and not-found, and scrolls away with the page. It is
an `<aside aria-label="Preview notice">` built from existing dark tokens (`--ink-deep`,
`--dkmut`, `--dkline`) with a mono label pill, stays on one line on mobile, and links to
`/legal`. Other pages keep a caveat only where it changes a decision.

`lib/launch.ts` holds `LAUNCH_DATE` and `FOUNDING_DEADLINE` with long-month formatters
("April 22, 2027"), plus `foundingDaysLeft(now)`, New York calendar days through December
31 and `null` after. Pages must use these rather than literal dates. The Go
`recognition.FoundingCutoff` stays the authoritative Founding rule.
