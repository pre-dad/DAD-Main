"use client";

import Link from "next/link";
import { useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { Page } from "@/components/donor/Page";
import { api, failureMessage, type RequestBody, type ResponseBody } from "@/lib/api";
import { useHashToken } from "@/lib/useHashToken";
import { useFocusOnSuccess } from "@/lib/useStepFocus";

const outcomes: Record<string, string> = {
  COMPLETED: "Your email has changed and all devices have been signed out.",
  NEW: "Current address verified. Check your new address for the second confirmation link. Your account email has not changed yet.",
};

export default function EmailChange() {
  const { token: proof, clear } = useHashToken();
  const [busy, setBusy] = useState(false);
  const [stage, setStage] = useState("");
  const [error, setError] = useState("");
  const heading = useFocusOnSuccess(stage !== "");
  async function confirm() {
    if (busy || !proof) return;
    setBusy(true);
    setError("");
    try {
      const result = await api<ResponseBody<"/donor/email-change/confirm", "post">>("/donor/email-change/confirm", {
        method: "POST",
        body: JSON.stringify({ token: proof } satisfies RequestBody<"/donor/email-change/confirm", "post">),
      });
      clear();
      setStage(result.stage);
    } catch (cause) {
      setError(failureMessage(cause, "The change was not confirmed. Please retry in a moment."));
    } finally {
      setBusy(false);
    }
  }
  return (
    <Page eyebrow="My account" title="Confirm your email change" heading={heading}>
      {stage === "COMPLETED" ? (
        <Link className="button" href="/account/sign-in">
          Sign in using your new address
        </Link>
      ) : stage ? null : (
        <>
          <p>
            Continue only if you requested this email change. Both your current and new addresses must be verified.
            Finishing the change signs out all devices.
          </p>
          {proof ? (
            <button type="button" className="button" disabled={busy} onClick={confirm}>
              {busy ? "Confirming…" : "Confirm this address"}
            </button>
          ) : (
            proof === "" && <p>Open the confirmation link from your email.</p>
          )}
          <p>
            <Link href="/account/profile">Request a new change from your profile</Link>
          </p>
        </>
      )}
      <Announcer status={stage ? (outcomes[stage] ?? "Your confirmation was received.") : ""} alert={error} />
    </Page>
  );
}
