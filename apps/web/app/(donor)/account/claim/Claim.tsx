"use client";

import Link from "next/link";
import { useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { Page } from "@/components/donor/Page";
import type { SignInReturn } from "@/components/donor/SignInForm";
import { api, failureMessage, type RequestBody } from "@/lib/api";
import { useHashToken } from "@/lib/useHashToken";
import { useFocusOnSuccess } from "@/lib/useStepFocus";

// Where an emailed link may send a donor once signed in (the API's own list). Anything else
// is ignored, so a crafted link cannot redirect anywhere else.
const destinations = new Set<string>([
  "/account/dashboard",
  "/account/payment-method",
  "/donate",
] satisfies SignInReturn[]);

export default function Claim() {
  const { token, next, clear } = useHashToken();
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState(false);
  const heading = useFocusOnSuccess(done);
  const [error, setError] = useState("");

  async function claim() {
    if (!token) return;
    setBusy(true);
    setError("");
    try {
      await api("/donor/claim", {
        method: "POST",
        body: JSON.stringify({ token } satisfies RequestBody<"/donor/claim", "post">),
      });
      clear();
      if (next && destinations.has(next)) return window.location.replace(next);
      setDone(true);
    } catch (cause) {
      setError(failureMessage(cause, "We couldn’t sign you in right now. Please retry in a moment."));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Page eyebrow="My account" title={done ? "You’re signed in" : "Claim your donor account"} heading={heading}>
      {done ? (
        <>
          <p className="lead">
            Your dashboard and giving streak are ready. Choose a public nickname whenever you’d like your referral link
            and code.
          </p>
          <Link className="button full" href="/account/dashboard">
            View my dashboard
          </Link>
          <p>
            <Link href="/account/payment-method">Update payment method</Link>
          </p>
        </>
      ) : (
        <>
          <p className="lead">Confirm to sign in to the donor account associated with this email link.</p>
          {token ? (
            <button type="button" className="button full" disabled={busy} onClick={claim}>
              {busy ? "Signing in…" : "Confirm and sign in"}
            </button>
          ) : (
            token === "" && (
              <p>This page needs the link from your email. Links work once and expire after 15 minutes.</p>
            )
          )}
          <p>
            <Link href="/account/sign-in">Request a new link</Link>
          </p>
        </>
      )}
      <Announcer status={done ? "You’re signed in." : ""} alert={error} />
    </Page>
  );
}
