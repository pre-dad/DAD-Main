"use client";
import Link from "next/link";
import { useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { Page } from "@/components/donor/Page";
import { PAGE_SIZE, type Schema } from "@/lib/api";
import { formatDay } from "@/lib/dates";
import { formatDollars } from "@/lib/money";
import { resourceMessages, useApiResource } from "@/lib/useApiResource";

export default function Initiatives() {
  const [offset, setOffset] = useState(0);
  const { resource, data, reload } = useApiResource<Schema<"StaffInitiativePage">>(
    `/donor/staff/initiatives?offset=${offset}`,
  );
  const messages = resourceMessages(resource, {
    loading: "Loading initiatives…",
    failure: "Initiatives could not be loaded right now. Please retry in a moment.",
    forbidden: "Initiatives require Impact Ops access.",
  });
  return (
    <Page wide eyebrow="Staff" back={{ href: "/staff", label: "Staff back office" }} title="Initiatives">
      <p>
        <Link className="button" href="/staff/initiatives/new">
          New initiative
        </Link>
      </p>
      {resource.state === "signedOut" && (
        <p>
          <Link className="button" href="/staff/sign-in">
            Staff sign-in
          </Link>
        </p>
      )}
      {data && (
        <>
          {data.initiatives.length ? (
            data.initiatives.map((initiative) => (
              <article key={initiative.id}>
                <h2>
                  <Link href={`/staff/initiatives/${encodeURIComponent(initiative.id)}`}>{initiative.title}</Link>
                </h2>
                <p className="note">
                  {initiative.publish_state === "PUBLISHED" ? "Published" : "Draft"} ·{" "}
                  {initiative.status === "ACTIVE" ? "Active" : "Complete"}
                  {!initiative.in_rotation && " · Excluded from rotation"} · {initiative.location} · Started{" "}
                  {formatDay(initiative.started_on)} · Committed {formatDollars(initiative.committed_minor)}
                </p>
              </article>
            ))
          ) : (
            <p>No initiatives yet.</p>
          )}
          {offset > 0 && (
            <button type="button" className="secondary" onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}>
              Newer initiatives
            </button>
          )}
          {data.next_offset !== null && (
            <button type="button" className="secondary" onClick={() => setOffset(data.next_offset ?? 0)}>
              Older initiatives
            </button>
          )}
        </>
      )}
      <Announcer status={messages.status} alert={messages.alert} />
      {(resource.state === "offline" || resource.state === "error") && (
        <button type="button" className="secondary" onClick={reload}>
          Retry loading
        </button>
      )}
    </Page>
  );
}
