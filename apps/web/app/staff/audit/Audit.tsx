"use client";
import Link from "next/link";
import { Announcer } from "@/components/donor/Announcer";
import { KeysetPager } from "@/components/donor/KeysetPager";
import { Page } from "@/components/donor/Page";
import type { Schema } from "@/lib/api";
import { formatDateTime } from "@/lib/dates";
import { resourceMessages } from "@/lib/useApiResource";
import { useKeysetResource } from "@/lib/useKeysetResource";

export default function Audit() {
  const { resource, data, reload, heading, newer, older } = useKeysetResource<Schema<"AuditPage">>(
    (cursor) => `/donor/staff/audit${cursor ? `?cursor=${encodeURIComponent(cursor)}` : ""}`,
  );
  const messages = resourceMessages(resource, {
    loading: "Loading audit history…",
    failure: "Audit history could not be loaded right now. Please retry in a moment.",
    forbidden: "Audit history requires administrator access.",
  });
  return (
    <Page
      wide
      eyebrow="Staff"
      back={{ href: "/staff", label: "Staff back office" }}
      title="Staff audit history"
      heading={heading}
    >
      {resource.state === "signedOut" && (
        <p>
          <Link className="button" href="/staff/sign-in">
            Staff sign-in
          </Link>
        </p>
      )}
      {data && (
        <>
          {data.events.length ? (
            <ol>
              {data.events.map((event) => (
                <li key={event.id}>
                  <time dateTime={event.at}>{formatDateTime(event.at)}</time> · {event.action}
                  <p>
                    Actor: {event.actor_id || "No authenticated actor (bootstrap or failed sign-in)"}. Target:{" "}
                    {event.target}
                  </p>
                  {Object.keys(event.details).length > 0 && (
                    <dl className="details">
                      {Object.entries(event.details).map(([key, value]) => (
                        <div key={key}>
                          <dt>{key.replaceAll("_", " ")}</dt>
                          <dd>
                            {typeof value === "string" || typeof value === "number" || typeof value === "boolean"
                              ? String(value)
                              : JSON.stringify(value)}
                          </dd>
                        </div>
                      ))}
                    </dl>
                  )}
                </li>
              ))}
            </ol>
          ) : (
            <p>No events recorded.</p>
          )}
          <KeysetPager newer={newer} older={older} labels={{ newer: "Newer events", older: "Older events" }} />
        </>
      )}
      <Announcer status={messages.status} alert={messages.alert} />
      {resource.state !== "signedOut" && (
        <button type="button" className="secondary" onClick={reload}>
          Refresh audit history
        </button>
      )}
    </Page>
  );
}
