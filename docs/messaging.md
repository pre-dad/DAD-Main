# R1 messaging contract

Messages currently identify the environment as a sandbox. They are not approved tax
receipts. Verify the receiving entity and EIN, obtain approved wording, and test the
configured provider before enabling live donations. SMTP delivery is at least once.

| Message | Trigger and audience | Preference treatment | Implementation |
|---|---|---|---|
| Gift receipt | Successful gift; giving donor | Essential financial record; no marketing content | `internal/store/receipts.go` and `messages.go` |
| Annual summary | Prior calendar year has gifts; giving donor | Essential financial record | `internal/store/receipts.go` and `outbox.go` |
| Payment recovery | Failed renewal, then day 5 of the 7-day grace window; giving donor | Essential payment notice; stop after recovery or cancellation | `internal/store/messages.go` and `outbox.go` |
| Referral converted | First qualifying referred gift; direct referrer | Optional; recheck referral-email preference before sending | `internal/store/messages.go` |
| Account claim/sign-in | Requested access or initial checkout claim; address owner | Essential authentication; single-use link, 15-minute expiry | `internal/store/email.go` |
| Email change | Signed-in request, then current-address confirmation; current/new address respectively | Essential security confirmation; two single-use proofs, each valid 15 minutes | `internal/store/email_change_delivery.go` |
| Passkey added | A passkey is registered; the account's address at send time | Essential security notice; names the passkey and when it was added, links to the profile, and is sent even if the passkey is removed first | `internal/store/donor_passkeys.go` and `messages.go` |
| Streak ended | Latest renewal still failed when the 7-day grace window ended; giving donor | Essential status notice; suppressed if the payment recovered or the gift was cancelled first | `internal/store/messages.go` and `outbox.go` |
| Event waitlist promotion | A waitlisted reservation becomes confirmed; the reserving donor | Transactional; suppressed once the reservation no longer holds | `internal/store/events.go` and `messages.go` |
| Event cancelled | Staff cancel an event; every donor holding or waitlisted for a place | Transactional; no opt-out | `internal/store/event_staff.go` and `messages.go` |
| Partner status | Partner approval, a verified figure published, an initiative marked complete, and a monthly summary; each recipient of the partner organisation | Operational notice to partner users, not donors; stopped if the partner is no longer approved | `internal/store/partner_push.go` and `partner_messages.go` |
| Partner invitation/sign-in | Partner approval or invitation (7-day invitation), or a sign-in request; the partner user | Essential authentication; single-use link | `internal/store/partner_sign_in.go` |
| Enrolment confirmation | Program submission; enrolled person | R2 activation only; final preference classification requires approval | Prepared copy below; no R1 sending trigger |

Implementation paths are relative to `apps/donor-api`. Marketing consent is independent
of referral notices. Treating financial and authentication messages as essential is the
current product rule; legal must approve that classification before launch. Staff resends
require permission and an audit trail. Delivery history distinguishes queued, completed,
failed, and suppressed messages; queued does not mean received.

## App notifications

The donor app receives push notifications through APNs and FCM
(`internal/store/push.go`), queued with the same facts as the messages above. Each is
optional (event cancellations excepted), start on, and are rechecked against current
preferences before sending. They name no one and no amount, and open the given app path.

| Notification | Trigger and audience | Text | Opens |
|---|---|---|---|
| Gift processed | Recent verified gift; giving donor, once per gift | "Your gift was processed" / "Your test gift was processed. Open the app to see your giving history." | `/account/gifts` |
| Referral converted | First qualifying referred gift; direct referrer | "Your invitation worked" / "Someone you invited through a Link Referral (an IRL Referral) just made their first test gift." | `/account/dashboard` |
| Streak at risk | Failed latest renewal while the streak is at risk, initially and on grace day 5; giving donor | "Your giving streak is at risk" / "Your monthly test payment was declined. Update your payment method to keep your streak going." | `/account/payment-method` |
| Streak milestone | Streak reaches an R1 day milestone, once per run; giving donor | "Your 100-day giving streak" / "You have kept your giving streak going for 100 days. Thank you for showing up every day." (staff may replace the wording per milestone) | `/account/dashboard` |
| Streak ended | Latest renewal still failed when grace ended; giving donor | "Your giving streak ended" / "Your monthly test payment did not recover in time, so your streak reset. Start a new streak whenever you are ready." | `/account/payment-method` |
| Badge earned | A badge is earned; the donor, dropped if staff revoke the badge first | "You earned a new badge" / "You just earned a badge for your giving. Open the app to see it." | `/account/dashboard` |
| Spot opened | A waitlisted reservation is promoted; the reserving donor | "A spot opened up" / "You are off the waitlist — your spot at an upcoming event is confirmed. Open the app for details." | `/account/dashboard` |
| Event nearby | An event is published in or near a volunteer's city; donors who volunteer | "A new event near you" / "A volunteer event was just announced near you. Open the app for the details." | `/account/dashboard` |
| Event reminder | 24 hours and 2 hours before an event the donor holds a place at | "Your event is coming up" / "An event you reserved is tomorrow." or "starts in about 2 hours." | `/account/dashboard` |
| Event cancelled | Staff cancel an event; donors holding or waitlisted for a place | "An event was cancelled" / "An event you signed up for has been cancelled. Open the app for the details." | `/account/dashboard` |

A notification not sent within 24 hours of becoming available is dropped rather than shown late.
The day-five reminder is queued with the failure, waits until five days after its due time,
and is suppressed after recovery, cancellation, or grace expiry. A pending initial reminder
is superseded once day five is due. Gift-processed notifications recheck the choice and
verified gift at delivery; a full refund suppresses them. Email receipts remain essential
when the donor turns off gift-processed pushes.

## Prepared enrolment confirmation

Subject: Your enrolment in {{program_name}}

Hello {{recipient_name}},

We have received your enrolment in {{program_name}}.

This message confirms your enrolment. It does not promise funds or any particular
amount of support. DAD will contact you about the program's next steps.

If you did not submit this enrolment, contact {{support_contact}}.

Dynamic Aid Delivery

This template is held for R2. Program rules, recipient identity, contact details, and
notification preferences must be defined before connecting it to an enrolment workflow.
Do not collect recipient records or activate this trigger in R1.
