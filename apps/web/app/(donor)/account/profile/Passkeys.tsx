"use client";

import { useEffect, useRef, useState } from "react";
import { ConfirmButton } from "@/components/ConfirmButton";
import { Announcer } from "@/components/donor/Announcer";
import { ApiError, api, failureMessage, type Schema } from "@/lib/api";
import { formatDate, formatDateTime } from "@/lib/dates";
import {
  type Ceremony,
  createPasskey,
  NO_SUITABLE_PASSKEY_MESSAGE,
  passkeyFailure,
  UNSUPPORTED_MESSAGE,
  usePasskeysSupported,
  WAITING_MESSAGE,
} from "@/lib/passkeys";
import { resourceMessages, useApiResource } from "@/lib/useApiResource";

type Passkey = Schema<"PasskeyInfo">;
type PasskeyList = Schema<"PasskeyList">;

const MAX_NAME = 64;
const label = (passkey: Passkey) => passkey.name || "Passkey";

function explainAdd(cause: unknown): string {
  // Raised when this authenticator already holds one of the excluded passkeys.
  if (cause instanceof DOMException && cause.name === "InvalidStateError")
    return "This device or password manager already has a passkey for your account.";
  switch (passkeyFailure(cause)) {
    case "cancelled":
      return "Adding a passkey was cancelled or timed out. Choose “Add a passkey” to try again.";
    case "unsupported":
      return NO_SUITABLE_PASSKEY_MESSAGE;
    case "refused":
      return "Your browser refused the passkey request. Please try again.";
  }
  return failureMessage(cause, "The passkey could not be added right now. Please retry in a moment.");
}

/** Passkeys that sign the donor in without an email link, with a way to add or remove them. */
export function Passkeys({ onSignedOut }: { onSignedOut: () => void }) {
  const supported = usePasskeysSupported(true);
  const { resource, data, failed, reload, setData } = useApiResource<PasskeyList>("/donor/me/passkeys");
  const [busy, setBusy] = useState(false);
  const [prompting, setPrompting] = useState(false);
  const [name, setName] = useState("");
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");
  const heading = useRef<HTMLHeadingElement>(null);
  const buttons = useRef(new Map<string, HTMLButtonElement>());
  const focusAfterUpdate = useRef<string | null>(null);
  useEffect(() => {
    if (resource.state === "signedOut") onSignedOut();
  }, [resource.state, onSignedOut]);
  // A removed row takes its focused button with it: land on the row that took its place, or the heading.
  useEffect(() => {
    if (!data || focusAfterUpdate.current === null) return;
    (buttons.current.get(focusAfterUpdate.current) ?? heading.current)?.focus();
    focusAfterUpdate.current = null;
  }, [data]);
  const messages = resourceMessages(resource, {
    loading: "Loading your passkeys…",
    failure: "Your passkeys could not be loaded right now. Please retry in a moment.",
  });

  async function run(work: () => Promise<void>, explain: (cause: unknown) => string) {
    if (busy) return;
    setBusy(true);
    setError("");
    setMessage("");
    try {
      await work();
    } catch (cause) {
      setError(explain(cause));
    } finally {
      setBusy(false);
      setPrompting(false);
    }
  }
  const add = (event: React.FormEvent) => {
    event.preventDefault();
    const trimmed = name.trim();
    run(
      async () => {
        const { ceremony, options } = await api<Ceremony<PublicKeyCredentialCreationOptionsJSON>>(
          "/donor/me/passkeys/register/options",
          { method: "POST" },
        );
        setPrompting(true);
        const credential = await createPasskey(options);
        setPrompting(false);
        setData(
          await api<PasskeyList>("/donor/me/passkeys/register", {
            method: "POST",
            body: JSON.stringify(trimmed ? { ceremony, credential, name: trimmed } : { ceremony, credential }),
          }),
        );
        setName("");
        setMessage(`Added ${trimmed || "a passkey"}. You can now sign in with it instead of an email link.`);
      },
      (cause) => {
        if (!(cause instanceof ApiError && cause.status === 401)) return explainAdd(cause);
        // A refused passkey and an ended session are both 401; reloading the list tells them apart.
        reload();
        return "That passkey wasn’t accepted. Try again, or use a different device or password manager.";
      },
    );
  };
  const remove = (passkey: Passkey) => {
    const position = data?.passkeys.indexOf(passkey) ?? 0;
    run(
      async () => {
        const updated = await api<PasskeyList>(`/donor/me/passkeys/${passkey.id}/delete`, { method: "POST" }).catch(
          (cause) => {
            // Already removed, perhaps from another device: what's left is what the donor needs to see.
            if (cause instanceof ApiError && cause.status === 404) return api<PasskeyList>("/donor/me/passkeys");
            throw cause;
          },
        );
        focusAfterUpdate.current = updated.passkeys[Math.min(position, updated.passkeys.length - 1)]?.id ?? "";
        setData(updated);
        setMessage(`Removed ${label(passkey)}. It can no longer sign in to your account.`);
      },
      (cause) => {
        if (cause instanceof ApiError && cause.status === 401) {
          onSignedOut();
          return "";
        }
        return failureMessage(cause, "Removal was not confirmed. Refresh the list to check before retrying.");
      },
    );
  };
  const retry = () => {
    setError("");
    setMessage("");
    reload();
  };

  // A deployment without passkeys offers none to add or manage.
  if (resource.state === "unconfigured") return null;
  return (
    <section aria-labelledby="passkeys-heading">
      <h2 id="passkeys-heading" ref={heading} tabIndex={-1}>
        Passkeys
      </h2>
      <p>
        Sign in with your fingerprint, face, screen lock, or security key instead of waiting for an email link. We email
        you whenever one is added.
      </p>
      {data && (
        <>
          {data.passkeys.length > 0 ? (
            <ul className="rows">
              {data.passkeys.map((passkey) => (
                <li key={passkey.id}>
                  <div className="session">
                    <p>
                      <strong>{label(passkey)}</strong>
                    </p>
                    <p className="note">
                      Added <time dateTime={passkey.created_at}>{formatDate(passkey.created_at)}</time> ·{" "}
                      {passkey.last_used_at ? (
                        <>
                          Last used <time dateTime={passkey.last_used_at}>{formatDateTime(passkey.last_used_at)}</time>
                        </>
                      ) : (
                        "Not used yet"
                      )}
                    </p>
                  </div>
                  <ConfirmButton
                    className="quiet danger"
                    disabled={busy}
                    prompt="It will no longer be able to sign in to your account."
                    confirmLabel="Confirm removal"
                    onConfirm={() => remove(passkey)}
                    target={`${label(passkey)}, added ${formatDate(passkey.created_at)}`}
                    buttonRef={(button) => {
                      if (button) buttons.current.set(passkey.id, button);
                      return () => {
                        buttons.current.delete(passkey.id);
                      };
                    }}
                  >
                    Remove
                  </ConfirmButton>
                </li>
              ))}
            </ul>
          ) : (
            <p className="note">You haven’t added a passkey yet.</p>
          )}
          {supported ? (
            <form onSubmit={add}>
              <label>
                Passkey name (optional)
                <input
                  type="text"
                  maxLength={MAX_NAME}
                  autoComplete="off"
                  value={name}
                  onChange={(event) => setName(event.target.value)}
                  disabled={busy}
                  aria-describedby="passkey-name-hint"
                />
              </label>
              <p className="note" id="passkey-name-hint">
                Helps you tell passkeys apart, like “Work laptop” or “Phone”.
              </p>
              {/* aria-disabled rather than disabled, so a cancelled prompt leaves focus on the button. */}
              <button type="submit" className="secondary" aria-disabled={busy || undefined}>
                Add a passkey
              </button>
            </form>
          ) : (
            <p className="callout">{UNSUPPORTED_MESSAGE}</p>
          )}
        </>
      )}
      <Announcer status={prompting ? WAITING_MESSAGE : messages.status || message} alert={error || messages.alert} />
      {(failed || error) && (
        <button type="button" className="secondary" disabled={busy} onClick={retry}>
          Refresh passkeys
        </button>
      )}
    </section>
  );
}
