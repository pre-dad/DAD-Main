"use client";

import Link from "next/link";
import { useEffect, useRef, useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { Page } from "@/components/donor/Page";
import { SignInPrompt } from "@/components/donor/SignInPrompt";
import { api, failureMessage, type Schema } from "@/lib/api";
import { formatDate } from "@/lib/dates";
import { formatMoney } from "@/lib/money";
import { resourceMessages, useApiResource } from "@/lib/useApiResource";

export default function Giving() {
  // Offsets of the pages opened so far, so "Newer gifts" returns to exactly where the donor came from.
  const [trail, setTrail] = useState([0]);
  const offset = trail[trail.length - 1];
  const [signedOut, setSignedOut] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const { resource, data, failed, reload } = useApiResource<Schema<"GiftsPage">>(
    signedOut ? null : `/donor/me/gifts?offset=${offset}`,
  );
  const heading = useRef<HTMLHeadingElement>(null);
  const paged = useRef<{ previous: typeof data } | null>(null);
  // Paging replaces the list the donor was reading; land them at the top of the new page.
  useEffect(() => {
    // A late effect from the previous response must not consume this request.
    if (data && paged.current && data !== paged.current.previous) {
      paged.current = null;
      heading.current?.focus();
    }
  }, [data]);
  const page = (next: number[]) => {
    paged.current = { previous: data };
    setTrail(next);
  };
  const messages = resourceMessages(resource, {
    loading: "Loading your giving…",
    failure: "Giving history could not be loaded right now. Please retry in a moment.",
  });
  async function logout() {
    setBusy(true);
    setError("");
    try {
      await api("/donor/logout", { method: "POST" });
      setSignedOut(true);
    } catch (cause) {
      setError(failureMessage(cause, "Sign-out was not confirmed. Please retry."));
    } finally {
      setBusy(false);
    }
  }
  return (
    <Page eyebrow="My account" title="My giving" back={{ href: "/account/dashboard", label: "My dashboard" }}>
      {signedOut || resource.state === "signedOut" ? (
        <SignInPrompt>Sign in to view your giving</SignInPrompt>
      ) : (
        <>
          {data && (
            <>
              <p>
                Signed in as <strong>{data.email}</strong>
              </p>
              <nav aria-label="Manage giving" className="links">
                <Link href="/account/recurring">Manage monthly giving</Link>
                <Link href="/account/payment-method">Update payment method</Link>
                <Link href="/account/receipts">Annual summaries</Link>
              </nav>
              <h2 ref={heading} tabIndex={-1}>
                Gifts
              </h2>
              {data.gifts.length === 0 ? (
                <p>No confirmed test gifts yet. Payment confirmation may take a moment.</p>
              ) : (
                <ul className="rows">
                  {data.gifts.map((gift) => (
                    <li key={gift.id}>
                      <span className="rowSummary">
                        <strong>{formatMoney(gift.amount_minor, gift.currency)}</strong>
                        {" · "}
                        {gift.frequency === "MONTHLY" ? "Monthly" : "One time"}
                        {" · "}
                        <time dateTime={gift.paid_at}>{formatDate(gift.paid_at)}</time>
                        {gift.refunded_minor ? (
                          <>
                            {" · "}Refunded {formatMoney(gift.refunded_minor, gift.currency)}
                          </>
                        ) : null}
                      </span>
                      <Link href={`/account/receipts?gift=${encodeURIComponent(gift.id)}`}>Gift record</Link>
                    </li>
                  ))}
                </ul>
              )}
              {(trail.length > 1 || data.next_offset !== null) && (
                <div className="actions">
                  {trail.length > 1 && (
                    <button type="button" className="secondary" onClick={() => page(trail.slice(0, -1))}>
                      Newer gifts
                    </button>
                  )}
                  {data.next_offset !== null && (
                    <button type="button" className="secondary" onClick={() => page([...trail, data.next_offset ?? 0])}>
                      Older gifts
                    </button>
                  )}
                </div>
              )}
              <p className="note">
                Sandbox history only: simulated payments, not tax receipts. Refunds are shown once Stripe confirms them.
              </p>
            </>
          )}
          <Announcer status={messages.status} alert={error || messages.alert} />
          {failed && (
            <button type="button" className="secondary" onClick={reload}>
              Refresh history
            </button>
          )}
          {data && (
            <button type="button" className="quiet" disabled={busy} onClick={logout}>
              {busy ? "Signing out…" : "Sign out"}
            </button>
          )}
        </>
      )}
    </Page>
  );
}
