"use client";

import { useRef, useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { GiftFraming } from "@/components/donor/GiftFraming";
import { Page } from "@/components/donor/Page";
import { SignInPrompt } from "@/components/donor/SignInPrompt";
import { ApiError, api, type RequestBody, type Schema, unconfirmedMessage } from "@/lib/api";
import { formatDate } from "@/lib/dates";
import { dailyGiftMinor, formatMoney, MAX_DAILY_DOLLARS, monthlyFraming } from "@/lib/money";
import { resourceMessages, useApiResource } from "@/lib/useApiResource";
import { useStepFocus } from "@/lib/useStepFocus";

type Change = Schema<"AmountChange">;

const presets = [1, 3, 5];

/** The daily dollars a monthly amount stands for, or "" when it predates the daily rule. */
const dailyInput = (minor: number) => (dailyGiftMinor(String(minor / 3000)) === minor ? String(minor / 3000) : "");

const confirmedCopy = (change: Change) =>
  `Last amount change confirmed: ${formatMoney(change.amount_minor)}/month from ${formatDate(change.effective_at)}.`;

export default function Amount() {
  const [amount, setAmount] = useState("1");
  const [confirmation, setConfirmation] = useState<Change | null>(null);
  const [requestKey, setRequestKey] = useState<string | null>(null);
  const [signedOut, setSignedOut] = useState(false);
  const [error, setError] = useState("");
  const [amountInvalid, setAmountInvalid] = useState(false);
  const { resource, data, failed, reload, setData } = useApiResource<Schema<"AmountOptions">>(
    signedOut ? null : "/donor/me/recurring/amount",
    {
      onLoad: (value) => {
        setRequestKey(null);
        setConfirmation(null);
        setError("");
        setAmountInvalid(false);
        if (value.state !== "ready") return;
        setAmount(dailyInput(value.amount_minor));
        if (value.change && !value.change.confirmed) {
          setConfirmation(value.change);
          setRequestKey(value.change.request_id);
        }
      },
    },
  );
  const [message, setMessage] = useState("");
  const [busy, setBusy] = useState(false);
  const amountInput = useRef<HTMLInputElement>(null);
  const step = useStepFocus(confirmation !== null);
  const typed = dailyGiftMinor(amount);
  const messages = resourceMessages(resource, {
    loading: "Checking monthly giving…",
    failure: "We could not load amount changes right now. Please refresh in a moment.",
  });

  function review(event: React.FormEvent) {
    event.preventDefault();
    setError("");
    setMessage("");
    setAmountInvalid(false);
    if (data?.state !== "ready") return;
    const minor = dailyGiftMinor(amount);
    const problem =
      minor === null
        ? `Enter a whole number of dollars a day from $1 to $${MAX_DAILY_DOLLARS}. Larger gifts are arranged with DAD directly.`
        : minor === data.amount_minor
          ? "That is already your daily gift. Choose a different one."
          : "";
    if (problem || minor === null) {
      setError(problem);
      setAmountInvalid(true);
      amountInput.current?.focus();
      return;
    }
    setConfirmation({
      request_id: "",
      amount_minor: minor,
      previous_minor: data.amount_minor,
      effective_at: data.effective_at,
      confirmed: false,
    });
  }
  async function save() {
    if (data?.state !== "ready" || !confirmation || busy) return;
    const key = requestKey ?? crypto.randomUUID();
    setRequestKey(key);
    setBusy(true);
    setError("");
    try {
      const result = await api<Change>("/donor/me/recurring/amount", {
        method: "POST",
        headers: { "Idempotency-Key": key },
        body: JSON.stringify({
          subscription_id: data.subscription_id,
          amount_minor: confirmation.amount_minor,
          previous_minor: confirmation.previous_minor,
          effective_at: confirmation.effective_at,
        } satisfies RequestBody<"/donor/me/recurring/amount", "post">),
      });
      setData({ ...data, amount_minor: result.amount_minor, change: result });
      setConfirmation(null);
      setRequestKey(null);
      setAmount(dailyInput(result.amount_minor));
      if (result.confirmed) setMessage(confirmedCopy(result));
    } catch (cause) {
      if (cause instanceof ApiError && cause.status === 401) setSignedOut(true);
      else
        setError(
          unconfirmedMessage(cause, "Amount change is not confirmed. Retry the same request or refresh its status."),
        );
    } finally {
      setBusy(false);
    }
  }
  return (
    <Page
      eyebrow="My account"
      title="Change your daily gift"
      back={{ href: "/account/recurring", label: "Manage monthly giving" }}
    >
      {signedOut || resource.state === "signedOut" ? (
        <SignInPrompt>Sign in to manage your giving</SignInPrompt>
      ) : (
        <>
          {data?.state === "unavailable" && (
            <p>
              Your monthly amount can’t be changed right now. Changes are available for an active gift that isn’t
              ending.
            </p>
          )}
          {data?.state === "ready" && (
            <>
              <p className="lead">
                Your gift from the next billing date: <GiftFraming minor={data.amount_minor} />
              </p>
              {data.change?.confirmed && !message && <p>{confirmedCopy(data.change)}</p>}
              {confirmation ? (
                <div className="callout">
                  <h2 ref={step.heading} tabIndex={-1}>
                    Confirm your new daily gift
                  </h2>
                  <p>
                    From <GiftFraming minor={confirmation.previous_minor} /> to{" "}
                    <GiftFraming minor={confirmation.amount_minor} />, starting {formatDate(confirmation.effective_at)}.
                  </p>
                  <p>
                    Past gifts and outstanding invoices keep their original amounts. Your streak continues with
                    successful monthly charges.
                  </p>
                  <div className="actions">
                    <button type="button" className="button" disabled={busy} onClick={save}>
                      {busy ? "Checking change…" : "Confirm amount change"}
                    </button>
                    <button
                      type="button"
                      className="secondary"
                      disabled={busy || requestKey !== null}
                      onClick={() => setConfirmation(null)}
                    >
                      Back
                    </button>
                  </div>
                </div>
              ) : (
                <form onSubmit={review}>
                  <div className="presets">
                    {presets.map((dollars) => {
                      const framing = monthlyFraming(dailyGiftMinor(String(dollars)) ?? 0);
                      return (
                        <button
                          key={dollars}
                          type="button"
                          aria-pressed={amount.trim() === String(dollars)}
                          onClick={() => {
                            setAmount(String(dollars));
                            setAmountInvalid(false);
                          }}
                        >
                          <span>{framing.daily}</span> · <span>{framing.monthly}</span>
                        </button>
                      );
                    })}
                  </div>
                  <label>
                    Dollars a day
                    <input
                      ref={amountInput}
                      inputMode="numeric"
                      value={amount}
                      required
                      aria-invalid={amountInvalid || undefined}
                      aria-describedby={amountInvalid ? "amount-error" : undefined}
                      onChange={(event) => {
                        setAmount(event.target.value);
                        setAmountInvalid(false);
                      }}
                    />
                  </label>
                  {typed !== null && typed !== data.amount_minor && (
                    <p>
                      New amount: <GiftFraming minor={typed} />
                    </p>
                  )}
                  <button type="submit" ref={step.trigger} className="button">
                    Review change
                  </button>
                </form>
              )}
            </>
          )}
          <p className="note">Sandbox only. This changes future test gifts, with no charge or proration today.</p>
          <Announcer id="amount-error" status={messages.status || message} alert={error || messages.alert} />
          {(failed || (requestKey !== null && error)) && (
            <button type="button" className="secondary" disabled={busy} onClick={reload}>
              Refresh amount details
            </button>
          )}
        </>
      )}
    </Page>
  );
}
