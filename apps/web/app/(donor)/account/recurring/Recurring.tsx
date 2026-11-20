"use client";

import Link from "next/link";
import { useRef, useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { Page } from "@/components/donor/Page";
import { SignInPrompt } from "@/components/donor/SignInPrompt";
import { ApiError, api, type RequestBody, type Schema, unconfirmedMessage } from "@/lib/api";
import { formatDate, formatDateTime } from "@/lib/dates";
import { formatMoney } from "@/lib/money";
import { resourceMessages, useApiResource } from "@/lib/useApiResource";
import { useFocusOnSuccess, useStepFocus } from "@/lib/useStepFocus";

type Subscription = Schema<"Subscription">;

const cancellable = (status: string) => ["active", "past_due", "unpaid"].includes(status);
const settingUp = (status: string) => status === "incomplete";
const preparingCopy = "We’re confirming your new monthly gift with Stripe. This usually takes a few seconds.";
const date = (instant: string) => <time dateTime={instant}>{formatDate(instant)}</time>;
const ENDING = [
  "Renewals cancelled. Your last monthly gift period ends ",
  "; no further gifts will be charged.",
] as const;
const endingCopy = (stops: string) => `${ENDING[0]}${formatDate(stops)}${ENDING[1]}`;

function Status({ subscription }: { subscription: Subscription }) {
  if (subscription.stops_at)
    return (
      <p>
        {ENDING[0]}
        {date(subscription.stops_at)}
        {ENDING[1]}
      </p>
    );
  if (settingUp(subscription.status)) return <p>Your monthly gift is still being set up. Refresh in a moment.</p>;
  // The contract lets the API add statuses; one this page does not know is not a renewing gift.
  if (!cancellable(subscription.status)) return <p>Your monthly gift is not active.</p>;
  return <p>Next renewal: {date(subscription.period_end)}.</p>;
}

export default function RecurringGiving() {
  const [signedOut, setSignedOut] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const command = useRef<(RequestBody<"/donor/me/recurring/cancel", "post"> & { key: string }) | null>(null);
  const { resource, data, failed, reload, setData } = useApiResource<Schema<"RecurringDetails">>(
    signedOut ? null : "/donor/me/recurring",
    {
      onLoad: () => {
        setConfirming(false);
        setError("");
        command.current = null;
      },
    },
  );
  const [error, setError] = useState("");
  const [message, setMessage] = useState("");
  const [busy, setBusy] = useState(false);
  const step = useStepFocus(confirming);
  const heading = useFocusOnSuccess(Boolean(message && data?.subscription?.stops_at));
  const messages = resourceMessages(resource, {
    loading: "Checking your monthly gift…",
    failure: "We could not confirm your billing details right now. Please refresh in a moment.",
  });

  async function cancel() {
    if (!data?.subscription || busy) return;
    command.current ??= {
      key: crypto.randomUUID(),
      subscription_id: data.subscription.id,
      period_end: data.subscription.period_end,
    };
    const { key, ...body } = command.current;
    setBusy(true);
    setError("");
    setMessage("");
    try {
      const result = await api<Schema<"RecurringDetails">>("/donor/me/recurring/cancel", {
        method: "POST",
        headers: { "Idempotency-Key": key },
        body: JSON.stringify(body),
      });
      setData(result);
      setConfirming(false);
      command.current = null;
      if (result.subscription?.stops_at) setMessage(endingCopy(result.subscription.stops_at));
    } catch (cause) {
      if (cause instanceof ApiError && cause.status === 401) setSignedOut(true);
      else
        setError(
          unconfirmedMessage(
            cause,
            "Cancellation is not confirmed. Retry to check its status, or refresh billing details before confirming again.",
          ),
        );
    } finally {
      setBusy(false);
    }
  }
  const subscription = data?.subscription;
  const preparing = data?.state === "preparing";
  const changeable = subscription && !subscription.stops_at && cancellable(subscription.status);
  return (
    <Page
      eyebrow="My account"
      title="My monthly giving"
      back={{ href: "/account/giving", label: "My giving" }}
      heading={heading}
    >
      {signedOut || resource.state === "signedOut" ? (
        <SignInPrompt>Sign in to manage your giving</SignInPrompt>
      ) : (
        <>
          {preparing && (
            <div className="callout">
              <p>{preparingCopy}</p>
              <button type="button" className="secondary" onClick={reload}>
                Refresh billing details
              </button>
            </div>
          )}
          {data?.state === "ready" && !subscription && (
            <div className="callout">
              <p>You have no monthly gift to manage.</p>
              <Link className="button" href="/donate">
                Start a monthly gift
              </Link>
            </div>
          )}
          {subscription && (
            <>
              <p className="metricValue">{formatMoney(subscription.amount_minor, subscription.currency)}/month</p>
              {!(message && subscription.stops_at) && <Status subscription={subscription} />}
              {data?.observed_at && (
                <p className="note">
                  Billing details last updated from Stripe{" "}
                  <time dateTime={data.observed_at}>{formatDateTime(data.observed_at)}</time>.
                </p>
              )}
              <nav aria-label="Manage monthly giving" className="links">
                <Link href="/account/payment-method">Update payment method</Link>
                {changeable && <Link href="/account/amount">Change your daily gift</Link>}
              </nav>
              {!subscription.stops_at &&
                cancellable(subscription.status) &&
                (confirming ? (
                  <div className="callout">
                    <h2 ref={step.heading} tabIndex={-1}>
                      Confirm cancellation
                    </h2>
                    <p>
                      Stop future renewals at {date(subscription.period_end)}? This does not refund past gifts or
                      forgive an outstanding invoice. Your streak ends at the first skipped renewal. An earned Founding
                      Donor badge stays yours.
                    </p>
                    <div className="actions">
                      <button type="button" className="secondary danger" disabled={busy} onClick={cancel}>
                        {busy ? "Checking cancellation…" : "Yes, stop renewals"}
                      </button>
                      <button type="button" className="secondary" disabled={busy} onClick={() => setConfirming(false)}>
                        Keep my monthly gift
                      </button>
                    </div>
                  </div>
                ) : (
                  <button type="button" ref={step.trigger} className="quiet" onClick={() => setConfirming(true)}>
                    Cancel monthly giving
                  </button>
                ))}
            </>
          )}
          <Announcer
            status={messages.status || message || (preparing ? preparingCopy : "")}
            statusShownElsewhere={preparing && !message}
            alert={error || messages.alert}
          />
          {(failed || error || (subscription && settingUp(subscription.status))) && (
            <button type="button" className="secondary" disabled={busy} onClick={reload}>
              Refresh billing details
            </button>
          )}
          <p className="note">
            Sandbox only. These controls affect test subscriptions. Tax receipts are not available in this preview yet.
          </p>
        </>
      )}
    </Page>
  );
}
