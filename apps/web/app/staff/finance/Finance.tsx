"use client";
import Link from "next/link";
import { useRef, useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { Page } from "@/components/donor/Page";
import { ApiError, api, classifyFailure, type RequestBody, type Schema, unconfirmedMessage } from "@/lib/api";
import { formatDate, formatDateTime } from "@/lib/dates";
import { formatMoney, parseDollars } from "@/lib/money";
import { resourceMessages, useApiResource } from "@/lib/useApiResource";
import { useStepFocus } from "@/lib/useStepFocus";

type Quote = Schema<"RefundQuote"> | Schema<"RecurringDetails">;
type Confirmation =
  | RequestBody<"/donor/staff/donors/{donor_id}/gifts/{gift_id}/refund", "post">
  | RequestBody<"/donor/staff/donors/{donor_id}/cancellation", "post">;
type Result = Partial<Pick<Schema<"StaffRefund">, "status"> & Pick<Schema<"StaffCancellation">, "subscription">>;
type Loaded = { quote: Quote; pending: Schema<"StaffAction"> | null };

export default function Finance({ donor, gift }: { donor: string; gift: string }) {
  const path = `/donor/staff/donors/${encodeURIComponent(donor)}/${gift ? `gifts/${encodeURIComponent(gift)}/refund` : "cancellation"}`;
  const [amount, setAmount] = useState("");
  const [reason, setReason] = useState("");
  const [confirmation, setConfirmation] = useState<Confirmation | null>(null);
  const [requestKey, setRequestKey] = useState<string | null>(null);
  const [error, setError] = useState("");
  const [message, setMessage] = useState("");
  const [busy, setBusy] = useState(false);
  const [invalid, setInvalid] = useState<"amount" | "reason" | null>(null);
  const { resource, data, reload } = useApiResource<Loaded>(donor ? path : null, {
    load: async (signal) => {
      const quote = await api<Quote>(path, { signal });
      const target = "gift_id" in quote ? quote.gift_id : quote.subscription?.id;
      const kind = "gift_id" in quote ? "REFUND" : "CANCEL";
      const pending = target
        ? (
            await api<Schema<"PendingAction">>(
              `/donor/staff/donors/${encodeURIComponent(donor)}/actions/pending?${new URLSearchParams({ target_id: target, kind })}`,
              { signal },
            )
          ).action
        : null;
      if (
        pending &&
        ((pending.kind === "REFUND" && pending.amount_minor === null) ||
          (pending.kind === "CANCEL" && pending.period_end === null))
      ) {
        throw new ApiError("The saved financial action is incomplete.", 500);
      }
      return { quote, pending };
    },
    onLoad: ({ quote, pending }) => {
      setConfirmation(null);
      setRequestKey(null);
      setError("");
      setInvalid(null);
      if ("gift_id" in quote) setAmount((quote.available_minor / 100).toFixed(2));
      if (!pending) return;
      setRequestKey(pending.id);
      setReason(pending.reason);
      if (pending.kind === "REFUND" && pending.amount_minor !== null)
        setConfirmation({ amount_minor: pending.amount_minor, reason: pending.reason });
      else if (pending.kind === "CANCEL" && pending.period_end !== null)
        setConfirmation({ subscription_id: pending.target_id, period_end: pending.period_end, reason: pending.reason });
    },
  });
  const quote = data?.quote;
  const amountInput = useRef<HTMLInputElement>(null);
  const reasonInput = useRef<HTMLTextAreaElement>(null);
  const step = useStepFocus(confirmation !== null);
  const messages = resourceMessages(resource, {
    loading: "Loading financial records…",
    failure: "Financial details could not be loaded. Please retry.",
    forbidden: "Your staff role does not permit refunds or cancellations.",
  });

  function reject(field: "amount" | "reason", text: string) {
    setError(text);
    setInvalid(field);
    (field === "amount" ? amountInput : reasonInput).current?.focus();
  }
  function review(event: React.FormEvent) {
    event.preventDefault();
    setError("");
    setMessage("");
    setInvalid(null);
    if (!quote) return;
    const audit = reason.trim();
    const shortReason = () => reject("reason", "Enter an audit reason of at least five characters.");
    if ("gift_id" in quote) {
      const minor = parseDollars(amount);
      if (minor === null) return reject("amount", "Enter a refund in USD with at most two decimal places.");
      if (minor < 1 || minor > quote.available_minor)
        return reject("amount", "Choose an amount within the available refund balance.");
      if (audit.length < 5) return shortReason();
      setConfirmation({ amount_minor: minor, reason: audit });
    } else if (quote.subscription) {
      if (audit.length < 5) return shortReason();
      setConfirmation({
        subscription_id: quote.subscription.id,
        period_end: quote.subscription.period_end,
        reason: audit,
      });
    }
  }
  async function submit() {
    if (!confirmation || busy) return;
    const key = requestKey ?? crypto.randomUUID();
    setRequestKey(key);
    setBusy(true);
    setError("");
    setMessage("");
    try {
      const result = await api<Result>(path, {
        method: "POST",
        headers: { "Idempotency-Key": key },
        body: JSON.stringify(confirmation),
      });
      if (!result.status && !result.subscription?.stops_at) {
        setError("This action is not confirmed. Retry the same request to reconcile it.");
        return;
      }
      setMessage(
        result.status === "NOT_SUBMITTED"
          ? "No refund submitted. The available amount changed. Refresh and confirm a new request."
          : result.status
            ? `Stripe refund status: ${result.status}. Only successful refunds reduce collected totals.`
            : `Cancellation confirmed: renewals stop ${formatDate(result.subscription?.stops_at ?? "")}. Past gifts are not refunded.`,
      );
      reload();
    } catch (cause) {
      // Refused outright, so nothing is pending and the next attempt is a new request.
      if (classifyFailure(cause) === "unconfigured") setRequestKey(null);
      setError(
        unconfirmedMessage(
          cause,
          "This action is not confirmed. Retry the same request to reconcile it. Do not issue another refund while its outcome is unknown.",
        ),
      );
    } finally {
      setBusy(false);
    }
  }
  const describedBy = (field: "amount" | "reason") =>
    invalid === field ? { "aria-invalid": true, "aria-describedby": "finance-error" } : {};
  return (
    <Page
      wide
      eyebrow="Staff"
      back={{ href: "/staff", label: "Staff back office" }}
      title={gift ? "Refund a gift" : "Cancel monthly giving"}
    >
      <p>Sandbox only. A named financial permission and an audit reason are required.</p>
      {!donor && <p>Select a donor and gift in the staff back office.</p>}
      {resource.state === "signedOut" && donor && (
        <p>
          <Link className="button" href="/staff/sign-in">
            Staff sign-in
          </Link>
        </p>
      )}
      {quote && (
        <>
          {"gift_id" in quote ? (
            <p>
              Original gift: {formatMoney(quote.amount_minor)}. Available to refund:{" "}
              {formatMoney(quote.available_minor)} USD.
              {quote.observed_at && ` Last refund recorded ${formatDateTime(quote.observed_at)}.`}
            </p>
          ) : quote.subscription ? (
            <>
              <p>
                {formatMoney(quote.subscription.amount_minor)}/month.{" "}
                {quote.subscription.stops_at
                  ? `Renewals stop ${formatDate(quote.subscription.stops_at)}.`
                  : `Next renewal: ${formatDate(quote.subscription.period_end)}.`}
              </p>
              {quote.observed_at && (
                <p className="note">
                  Last updated from Stripe <time dateTime={quote.observed_at}>{formatDateTime(quote.observed_at)}</time>
                  .
                </p>
              )}
            </>
          ) : (
            <p>No monthly gift is available.</p>
          )}
          {confirmation ? (
            <div className="callout">
              <h2 ref={step.heading} tabIndex={-1}>
                Confirm financial action
              </h2>
              <p>
                {"amount_minor" in confirmation
                  ? `Refund ${formatMoney(confirmation.amount_minor)} USD to the original payment method.`
                  : `Stop future renewals at ${formatDate(confirmation.period_end)}. No refund is included.`}
              </p>
              <p>Reason: {confirmation.reason}</p>
              <div className="actions">
                <button type="button" className="secondary danger" disabled={busy} onClick={submit}>
                  {busy
                    ? "Checking action…"
                    : requestKey
                      ? "Retry original financial action"
                      : "Confirm financial action"}
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
            (("gift_id" in quote && quote.available_minor > 0) ||
              ("subscription" in quote && quote.subscription && !quote.subscription.stops_at)) && (
              <form onSubmit={review}>
                {"gift_id" in quote && (
                  <label>
                    Refund in USD
                    <input
                      ref={amountInput}
                      inputMode="decimal"
                      value={amount}
                      {...describedBy("amount")}
                      onChange={(event) => {
                        setAmount(event.target.value);
                        setInvalid(null);
                      }}
                      required
                    />
                  </label>
                )}
                <label>
                  Audit reason
                  <textarea
                    ref={reasonInput}
                    minLength={5}
                    maxLength={500}
                    value={reason}
                    {...describedBy("reason")}
                    onChange={(event) => {
                      setReason(event.target.value);
                      setInvalid(null);
                    }}
                    required
                  />
                </label>
                <button ref={step.trigger} className="button" disabled={busy} type="submit">
                  Review financial action
                </button>
              </form>
            )
          )}
        </>
      )}
      <Announcer
        id="finance-error"
        status={donor ? messages.status || message : ""}
        alert={donor ? error || messages.alert : ""}
      />
      {donor && resource.state !== "signedOut" && (
        <button type="button" className="secondary" disabled={busy} onClick={reload}>
          Refresh financial details
        </button>
      )}
    </Page>
  );
}
