"use client";

import { useEffect, useRef, useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { FoundingLine } from "@/components/donor/FoundingLine";
import { GiftFraming } from "@/components/donor/GiftFraming";
import { Page } from "@/components/donor/Page";
import { ApiError, api, failureMessage, type RequestBody, type Schema } from "@/lib/api";
import { dailyGiftMinor, MAX_DAILY_DOLLARS, monthlyFraming } from "@/lib/money";

const presets = [1, 3, 5];
// Remembers this tab's last choice so cancelling Stripe checkout returns to it.
const CHOICE_KEY = "dad.donate.choice";
type Choice = { amount: string; email: string; name?: string; zip?: string; city?: string };

function remembered(): Choice | null {
  try {
    const value = JSON.parse(sessionStorage.getItem(CHOICE_KEY) ?? "null");
    return typeof value?.amount === "string" && typeof value.email === "string" ? value : null;
  } catch {
    return null;
  }
}

export default function Donate({ verifiedEmail, onSignedOut }: { verifiedEmail: string; onSignedOut?: () => void }) {
  const email = verifiedEmail;
  const [name, setName] = useState("");
  const [zip, setZip] = useState("");
  const [city, setCity] = useState("");
  const [amount, setAmount] = useState("1");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [amountInvalid, setAmountInvalid] = useState(false);
  const amountInput = useRef<HTMLInputElement>(null);
  const attempt = useRef<{ payload: string; key: string } | null>(null);
  const chosen = dailyGiftMinor(amount);
  // "Other" stays open while the donor types, and is also forced open for any amount that is
  // not a preset (a restored or invalid one), so rejectAmount's focus never targets an
  // unmounted input.
  const [otherOpen, setOtherOpen] = useState(false);
  const isPreset = presets.some((dollars) => amount.trim() === String(dollars));
  const showOther = otherOpen || !isPreset;

  useEffect(() => {
    const choice = remembered();
    if (choice && choice.email === verifiedEmail) {
      setAmount(choice.amount);
      setName(choice.name ?? "");
      setZip(choice.zip ?? "");
      setCity(choice.city ?? "");
    }
    // Returning with Back from Stripe can restore this page from the back/forward cache
    // mid-redirect; it must not stay stuck on "Opening secure checkout".
    const reopen = (event: PageTransitionEvent) => {
      if (event.persisted) setBusy(false);
    };
    window.addEventListener("pageshow", reopen);
    return () => window.removeEventListener("pageshow", reopen);
  }, [verifiedEmail]);

  // Moves focus only after the donor asks for "Other"; a restored amount must not steal it.
  useEffect(() => {
    if (otherOpen) amountInput.current?.focus();
  }, [otherOpen]);
  function edit(update: () => void) {
    update();
    setError("");
    setAmountInvalid(false);
  }

  function rejectAmount(message: string) {
    setError(message);
    setAmountInvalid(true);
    amountInput.current?.focus();
  }

  async function submit(event: React.FormEvent) {
    event.preventDefault();
    if (busy) return;
    setError("");
    setAmountInvalid(false);
    const minor = dailyGiftMinor(amount);
    if (minor === null)
      return rejectAmount(
        `Enter a whole number of dollars a day from $1 to $${MAX_DAILY_DOLLARS}. Larger gifts are arranged with DAD directly.`,
      );
    const payload = JSON.stringify({
      email,
      name,
      zip,
      city,
      amount_minor: minor,
      frequency: "MONTHLY",
    } satisfies RequestBody<"/donor/checkouts", "post">);
    if (attempt.current?.payload !== payload) attempt.current = { payload, key: crypto.randomUUID() };
    try {
      sessionStorage.setItem(CHOICE_KEY, JSON.stringify({ amount, email, name, zip, city } satisfies Choice));
    } catch {}
    setBusy(true);
    let destination: URL;
    try {
      const result = await api<Schema<"CheckoutResult">>("/donor/checkouts", {
        method: "POST",
        headers: { "Idempotency-Key": attempt.current.key },
        body: payload,
      });
      destination = new URL(result.url);
    } catch (cause) {
      if (cause instanceof ApiError && cause.status === 401) onSignedOut?.();
      setError(failureMessage(cause, "Checkout could not be opened. Please retry."));
      setBusy(false);
      return;
    }
    if (
      destination.protocol !== "https:" ||
      destination.host !== "checkout.stripe.com" ||
      destination.username ||
      destination.password
    ) {
      setError("Checkout returned an unexpected destination. No payment was started; please retry.");
      setBusy(false);
      return;
    }
    window.location.assign(destination.href);
  }

  // autoComplete="off" stops the browser restoring stale control values on Back, which
  // would show one frequency while React state holds the other.
  return (
    <Page eyebrow="Give" title="Join the beginning">
      <p className="lead">A small monthly gift, securely linked to your verified donor account.</p>
      <form className="giveForm" onSubmit={submit} autoComplete="off">
        <fieldset disabled={busy}>
          <legend>1 · Your daily gift</legend>
          <div className="presets">
            {presets.map((dollars) => {
              const { monthly: perMonth, daily } = monthlyFraming(dailyGiftMinor(String(dollars)) ?? 0);
              const pressed = !showOther && amount.trim() === String(dollars);
              return (
                <button
                  key={dollars}
                  type="button"
                  aria-pressed={pressed}
                  aria-label={`${daily} · ${perMonth}`}
                  onClick={() => {
                    setOtherOpen(false);
                    edit(() => setAmount(String(dollars)));
                  }}
                >
                  <span>{daily}</span>
                  <span>{perMonth}</span>
                  {pressed && <Check />}
                </button>
              );
            })}
            <button type="button" aria-pressed={showOther} onClick={() => setOtherOpen(true)}>
              <span>Other</span>
              <span>your amount</span>
              {showOther && <Check />}
            </button>
          </div>
          {showOther && (
            <label>
              Dollars a day
              <input
                ref={amountInput}
                inputMode="numeric"
                required
                value={amount}
                aria-invalid={amountInvalid || undefined}
                aria-describedby={
                  amountInvalid ? "donate-error" : chosen ? "donate-amount donate-amount-hint" : "donate-amount-hint"
                }
                onChange={(event) => {
                  // Pin Other open once the donor edits: typing "1", "3" or "5" on the way to
                  // "12" would otherwise make the amount a preset and unmount the field mid-keystroke.
                  setOtherOpen(true);
                  edit(() => setAmount(event.target.value));
                }}
              />
            </label>
          )}
          {showOther && (
            <p id="donate-amount-hint" className="note fieldHint">
              Up to ${MAX_DAILY_DOLLARS} a day
            </p>
          )}
          {chosen && (
            <p id="donate-amount" className="giveSummary">
              You’ll give <GiftFraming minor={chosen} />
            </p>
          )}
        </fieldset>
        <fieldset disabled={busy}>
          <legend>2 · Where we send your receipt</legend>
          <label>
            Email
            <input
              type="email"
              autoComplete="email"
              maxLength={320}
              required
              value={email}
              disabled={busy}
              aria-describedby="donate-email-hint"
              readOnly
            />
          </label>
          <p id="donate-email-hint" className="note fieldHint">
            Your verified account email receives the receipt. Change it in your profile.
          </p>
          <label>
            Full name
            <input
              autoComplete="name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              maxLength={150}
              disabled={busy}
              required
            />
          </label>
          <div className="fieldRow">
            <label>
              ZIP code
              <input
                autoComplete="postal-code"
                inputMode="numeric"
                value={zip}
                onChange={(e) => setZip(e.target.value)}
                pattern="[0-9]{5}(-[0-9]{4})?"
                disabled={busy}
                required
              />
            </label>
            <label>
              City
              <input
                autoComplete="address-level2"
                value={city}
                onChange={(e) => setCity(e.target.value)}
                maxLength={80}
                disabled={busy}
                required
              />
            </label>
          </div>
        </fieldset>
        {/* Sticky inside the form (never fixed) so it rides the bottom edge on phones without
            leaving the form's own flow; the Announcer sits in it so an error is never
            scrolled out of sight behind the button. */}
        <div className="giveSubmit">
          <Announcer id="donate-error" status={busy ? "Opening secure checkout…" : ""} alert={error} />
          <button className="button full" disabled={busy} type="submit">
            {busy ? (
              "Opening secure checkout…"
            ) : (
              <>
                <svg aria-hidden="true" viewBox="0 0 24 24" width="18" height="18" focusable="false">
                  <rect x="5" y="11" width="14" height="9" rx="2" fill="currentColor" />
                  <path d="M8 11V8a4 4 0 0 1 8 0v3" fill="none" stroke="currentColor" strokeWidth="2" />
                </svg>
                {chosen
                  ? `Continue to secure checkout · ${monthlyFraming(chosen).monthly}`
                  : "Continue to secure checkout"}
              </>
            )}
          </button>
        </div>
        <p className="note giveNotes">
          Preview: test payments only. Use a Stripe test card; no real money moves.
          <br />
          Card details go to Stripe, never to DAD.
        </p>
        <div className="giveFounding">
          <FoundingLine />
        </div>
      </form>
    </Page>
  );
}
// Selected-tile mark: shape, not colour alone, shows which amount is chosen.
function Check() {
  return (
    <svg className="presetCheck" aria-hidden="true" viewBox="0 0 24 24" width="18" height="18" focusable="false">
      <path d="M5 12.5l4.5 4.5L19 7.5" fill="none" stroke="currentColor" strokeWidth="3" />
    </svg>
  );
}
