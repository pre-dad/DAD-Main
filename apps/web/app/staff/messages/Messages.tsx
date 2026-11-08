"use client";
import Link from "next/link";
import { useRef, useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { Page } from "@/components/donor/Page";
import { api, type Schema, unconfirmedMessage } from "@/lib/api";
import { resourceMessages, useApiResource } from "@/lib/useApiResource";

export default function DeliveryHistory({ donor }: { donor: string }) {
  // Keyset paging: each entry is the cursor that opened a page, the first being none.
  const [cursors, setCursors] = useState<string[]>([]);
  const cursor = cursors.at(-1);
  const { resource, data, reload } = useApiResource<Schema<"MessagePage">>(
    donor
      ? `/donor/staff/donors/${encodeURIComponent(donor)}/messages${cursor ? `?cursor=${encodeURIComponent(cursor)}` : ""}`
      : null,
  );
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [message, setMessage] = useState("");
  const keys = useRef<Record<string, string>>({});
  // Only an unclaimed donor needs a claim email; until that is known, the option stays hidden.
  const { data: record } = useApiResource<Schema<"DonorDetail">>(
    donor ? `/donor/staff/donors/${encodeURIComponent(donor)}` : null,
  );
  const messages = resourceMessages(resource, {
    loading: "Loading delivery history…",
    failure: "Delivery history could not be loaded right now. Please retry in a moment.",
    forbidden: "Your staff role does not permit viewing delivery history.",
  });
  async function resend(id?: string) {
    if (busy) return;
    setBusy(true);
    setError("");
    setMessage("");
    try {
      if (id) {
        keys.current[id] ??= crypto.randomUUID();
        await api(`/donor/staff/messages/${id}/resend`, {
          method: "POST",
          headers: { "Idempotency-Key": keys.current[id] },
        });
      } else await api(`/donor/staff/donors/${encodeURIComponent(donor)}/claim-email`, { method: "POST" });
      setMessage("Delivery request recorded. This does not confirm email delivery.");
      reload();
    } catch (cause) {
      setError(
        unconfirmedMessage(
          cause,
          "Resend is not confirmed. Check whether the original is still queued, wait one minute, then retry the same request.",
        ),
      );
    } finally {
      setBusy(false);
    }
  }
  return (
    <Page wide eyebrow="Staff" back={{ href: "/staff", label: "Staff back office" }} title="Donor delivery history">
      {!donor && <p>Select a donor in the back office to view their messages.</p>}
      {resource.state === "signedOut" && donor && (
        <p>
          <Link className="button" href="/staff/sign-in">
            Staff sign-in
          </Link>
        </p>
      )}
      {data && (
        <>
          {record?.claimed === false && (
            <details>
              <summary>Request account claim email</summary>
              <p>This sends the donor a new account-access link if delivery is available.</p>
              <button type="button" className="secondary" disabled={busy} onClick={() => resend()}>
                Confirm claim email request
              </button>
            </details>
          )}
          {data.messages.length ? (
            <ul>
              {data.messages.map((item) => (
                <li key={item.id}>
                  {item.kind} ·{" "}
                  {item.suppressed_reason
                    ? `Suppressed: ${item.suppressed_reason}`
                    : item.completed_at
                      ? "Sent"
                      : item.attempts >= 5
                        ? "Parked for review"
                        : "Queued"}{" "}
                  · Attempts {item.attempts}
                  {item.last_error && <> · Last failure: {item.last_error}</>}
                  {["RECEIPT", "ANNUAL"].includes(item.kind) &&
                    !item.suppressed_reason &&
                    (item.completed_at || item.attempts >= 5) && (
                      <details>
                        <summary>Resend record</summary>
                        <p>Send a new copy showing current verified refund adjustments.</p>
                        <button type="button" className="secondary" disabled={busy} onClick={() => resend(item.id)}>
                          Confirm resend
                        </button>
                      </details>
                    )}
                </li>
              ))}
            </ul>
          ) : (
            <p>No transactional messages recorded.</p>
          )}
          {cursors.length > 0 && (
            <button type="button" className="secondary" onClick={() => setCursors(cursors.slice(0, -1))}>
              Newer messages
            </button>
          )}
          {data.next_cursor !== null && (
            <button
              type="button"
              className="secondary"
              onClick={() => setCursors([...cursors, data.next_cursor ?? ""])}
            >
              Older messages
            </button>
          )}
        </>
      )}
      <Announcer status={donor ? messages.status || message : ""} alert={donor ? error || messages.alert : ""} />
      {donor && resource.state !== "signedOut" && (
        <button type="button" className="secondary" disabled={busy} onClick={reload}>
          Refresh delivery history
        </button>
      )}
    </Page>
  );
}
