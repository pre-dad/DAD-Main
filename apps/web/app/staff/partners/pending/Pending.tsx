"use client";
import Link from "next/link";
import { useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { Page } from "@/components/donor/Page";
import { api, type ResponseBody } from "@/lib/api";
import { resourceMessages, useApiResource } from "@/lib/useApiResource";

type Pending = ResponseBody<"/donor/staff/partners/pending", "get">;

export default function Pending() {
  // Keyset paging: each entry is the cursor that opened a page, the first being none.
  const [cursors, setCursors] = useState<string[]>([]);
  const cursor = cursors.at(-1);
  const { resource, data, reload } = useApiResource<Pending>(
    `/donor/staff/partners/pending${cursor ? `?cursor=${encodeURIComponent(cursor)}` : ""}`,
  );
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [status, setStatus] = useState("");

  const messages = resourceMessages(resource, {
    loading: "Loading pending applications…",
    failure: "Pending applications could not be loaded right now. Please retry in a moment.",
    forbidden: "Partner administration requires partnerships access.",
  });

  async function decide(id: string, action: "approve" | "reject", org: string) {
    if (busy) return;
    setBusy(id);
    setError("");
    setStatus("");
    try {
      if (action === "reject") {
        const reason = window.prompt(`Reason for rejecting ${org}?`) ?? "";
        if (!reason.trim()) {
          setBusy("");
          return;
        }
        await api(`/donor/staff/partners/${id}/reject`, { method: "POST", body: JSON.stringify({ reason }) });
        setStatus(`Rejected ${org}.`);
      } else {
        await api(`/donor/staff/partners/${id}/approve`, { method: "POST", body: "{}" });
        setStatus(`Approved ${org}. The signer has been emailed a sign-in link.`);
      }
      reload();
    } catch {
      setError(`${org} could not be ${action === "approve" ? "approved" : "rejected"} — it may already be decided.`);
    } finally {
      setBusy("");
    }
  }

  return (
    <Page wide eyebrow="Staff" back={{ href: "/staff", label: "Staff back office" }} title="Partner applications">
      <p>
        <Link href="/staff/partners">Approved partners</Link>
      </p>
      {resource.state === "signedOut" && (
        <p>
          <Link className="button" href="/staff/sign-in">
            Staff sign-in
          </Link>
        </p>
      )}
      {messages.alert && <p className="callout">{messages.alert}</p>}
      {data &&
        (data.applications.length ? (
          data.applications.map((item) => (
            <article key={item.id}>
              <h2>{item.organization}</h2>
              <p className="note">
                {item.type} · {item.workforce_size.toLocaleString()} people · <a href={item.website}>{item.website}</a>
              </p>
              <p>
                {item.contact_name}, {item.title} · {item.work_email} · {item.phone}
              </p>
              <p>
                <button
                  type="button"
                  className="button"
                  disabled={busy === item.id}
                  onClick={() => decide(item.id, "approve", item.organization)}
                >
                  Approve
                </button>{" "}
                <button
                  type="button"
                  className="link"
                  disabled={busy === item.id}
                  onClick={() => decide(item.id, "reject", item.organization)}
                >
                  Reject
                </button>
              </p>
            </article>
          ))
        ) : (
          <p>{cursors.length ? "No more applications on this page." : "No applications are awaiting review."}</p>
        ))}
      {data && (
        <p className="actions">
          {cursors.length > 0 && (
            <button type="button" className="secondary" onClick={() => setCursors(cursors.slice(0, -1))}>
              Earlier applications
            </button>
          )}
          {data.next_cursor !== null && (
            <button
              type="button"
              className="secondary"
              onClick={() => setCursors([...cursors, data.next_cursor ?? ""])}
            >
              Later applications
            </button>
          )}
        </p>
      )}
      <Announcer status={status} alert={error} />
    </Page>
  );
}
