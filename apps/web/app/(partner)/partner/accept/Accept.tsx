"use client";
import Link from "next/link";
import { useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { Page } from "@/components/donor/Page";
import { api, failureMessage } from "@/lib/api";
import { useHashToken } from "@/lib/useHashToken";

export default function Accept() {
  const { token, clear } = useHashToken();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState(false);

  async function accept() {
    if (!token || busy) return;
    setBusy(true);
    setError("");
    try {
      await api("/donor/partner/accept-invite", { method: "POST", body: JSON.stringify({ invite: token }) });
      setDone(true);
      clear();
      window.location.assign("/partner");
    } catch (cause) {
      setError(failureMessage(cause, "We couldn’t sign you in right now. Please retry in a moment."));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Page eyebrow="Partner portal" title="Sign in to your partner portal">
      <p>Confirm to sign in to the partner account associated with this email link.</p>
      {done ? (
        <p>Signed in, redirecting…</p>
      ) : token ? (
        <button type="button" className="button" onClick={accept} disabled={busy}>
          {busy ? "Signing in…" : "Confirm and sign in"}
        </button>
      ) : (
        token === "" && <p>This page needs the link from your email. Open that link again, or request a new one.</p>
      )}
      <p>
        <Link href="/partner/sign-in">Email me a new sign-in link</Link>
      </p>
      <Announcer status={busy ? "Signing in…" : ""} alert={error} />
    </Page>
  );
}
