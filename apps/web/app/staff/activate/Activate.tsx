"use client";
import Link from "next/link";
import { useCallback, useEffect, useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { Page } from "@/components/donor/Page";
import { ApiError, api, classifyFailure, failureMessage, type Schema } from "@/lib/api";
import {
  type Ceremony,
  createPasskey,
  NO_SUITABLE_PASSKEY_MESSAGE,
  passkeyFailure,
  UNSUPPORTED_MESSAGE,
  usePasskeysSupported,
  WAITING_MESSAGE,
} from "@/lib/passkeys";
import { useHashToken } from "@/lib/useHashToken";

type Invitation = Ceremony<PublicKeyCredentialCreationOptionsJSON> & Pick<Schema<"Invitation">, "email" | "name">;

// The server keeps a ceremony for five minutes. An older one is replaced before the passkey
// is made, so a slow reader is not left holding a passkey the server will refuse.
const FRESH_FOR_MS = 3 * 60_000;

const general = (cause: unknown, fallback: string) =>
  ["offline", "rateLimited", "unconfigured"].includes(classifyFailure(cause))
    ? failureMessage(cause, fallback)
    : fallback;

const USED_INVITATION =
  "This invitation has already been used or has expired. If you already created a passkey, sign in with it. Otherwise, ask a DAD administrator for a new invitation.";

function explainCreation(cause: unknown): string {
  switch (passkeyFailure(cause)) {
    case "cancelled":
      return "Passkey creation was cancelled or timed out. Choose “Create a passkey” to try again.";
    case "unsupported":
      return NO_SUITABLE_PASSKEY_MESSAGE;
    case "refused":
      return "Your browser refused to create the passkey. Open the invitation link directly in your browser, then try again.";
  }
  return cause instanceof ApiError && cause.status === 401
    ? "The passkey wasn’t accepted. Try again: an invitation allows five attempts and expires 24 hours after it was sent."
    : general(cause, "Activation could not be completed right now. Please try again.");
}

export default function Activate() {
  const { token, clear } = useHashToken("dad.staff.invitation");
  const supported = usePasskeysSupported(true);
  const [invitation, setInvitation] = useState<{ value: Invitation; at: number } | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [used, setUsed] = useState(false);
  const [done, setDone] = useState(false);
  // A refused invitation is forgotten, so this tab no longer offers it.
  const refused = useCallback(
    (cause: unknown) => {
      if (!(cause instanceof ApiError && cause.status === 401)) {
        setError(general(cause, "Your invitation could not be checked right now. Reload this page to try again."));
        return;
      }
      setUsed(true);
      setError(USED_INVITATION);
      clear();
    },
    [clear],
  );
  const load = useCallback(async () => {
    const loaded = {
      value: await api<Invitation>("/donor/staff/activate/options", {
        method: "POST",
        body: JSON.stringify({ token }),
      }),
      at: Date.now(),
    };
    setInvitation(loaded);
    return loaded;
  }, [token]);
  useEffect(() => {
    if (!token) return;
    let current = true;
    load().catch((cause) => {
      if (current) refused(cause);
    });
    return () => {
      current = false;
    };
  }, [token, load, refused]);
  async function create() {
    if (busy || !invitation) return;
    setBusy(true);
    setError("");
    let value = invitation.value;
    if (Date.now() - invitation.at >= FRESH_FOR_MS) {
      try {
        value = (await load()).value;
      } catch (cause) {
        setBusy(false);
        refused(cause);
        return;
      }
    }
    let answered = false;
    try {
      const credential = await createPasskey(value.options);
      answered = true;
      await api("/donor/staff/activate", {
        method: "POST",
        body: JSON.stringify({ ceremony: value.ceremony, credential }),
      });
      setDone(true);
      clear();
      window.location.assign("/staff");
    } catch (cause) {
      setError(explainCreation(cause));
      setBusy(false);
      // An answered ceremony is spent, so another try needs a new one.
      if (answered) load().catch(refused);
    }
  }
  return (
    <Page eyebrow="Staff" title="Activate your staff account">
      {done ? (
        <p>Account activated, redirecting…</p>
      ) : used ? null : token === "" ? (
        <p className="callout">
          Open this page from the private invitation link a DAD administrator sent you. If the link has expired, ask
          them for a replacement invitation.
        </p>
      ) : (
        invitation && (
          <>
            <p>
              This private invitation is for <strong>{invitation.value.name}</strong> ({invitation.value.email}). If
              that isn’t you, close this page and tell a DAD administrator.
            </p>
            <p>
              Create a passkey to finish setting up your account. Your device or password manager saves it, then asks
              for your fingerprint, face, screen lock, or security key each time you sign in. There is no password to
              remember.
            </p>
            {supported ? (
              // aria-disabled rather than disabled, so a cancelled prompt leaves focus on the button.
              <button type="button" className="button" aria-disabled={busy || undefined} onClick={create}>
                Create a passkey
              </button>
            ) : (
              <p className="callout">{UNSUPPORTED_MESSAGE}</p>
            )}
          </>
        )
      )}
      <Announcer
        status={busy ? WAITING_MESSAGE : token && !invitation && !error ? "Checking your invitation…" : ""}
        alert={error}
      />
      {used && (
        <p>
          <Link className="button" href="/staff/sign-in">
            Staff sign-in
          </Link>
        </p>
      )}
    </Page>
  );
}
