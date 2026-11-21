"use client";
import Link from "next/link";
import { useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { Page } from "@/components/donor/Page";
import { PAGE_SIZE, type Schema } from "@/lib/api";
import { formatDate } from "@/lib/dates";
import { formatDollars } from "@/lib/money";
import { resourceMessages, useApiResource } from "@/lib/useApiResource";

export function matchTerms(rateBP: number, capMinor: number): string {
  if (rateBP === 0) return "No match";
  const rate = `${(rateBP / 100).toLocaleString("en-US", { maximumFractionDigits: 2 })}% match`;
  return capMinor > 0 ? `${rate} up to ${formatDollars(capMinor)} a year` : `${rate}, uncapped`;
}

export default function Partners() {
  const [offset, setOffset] = useState(0);
  const { resource, data, reload } = useApiResource<Schema<"StaffPartnerPage">>(
    `/donor/staff/partners?offset=${offset}`,
  );
  const messages = resourceMessages(resource, {
    loading: "Loading partners…",
    failure: "Partners could not be loaded right now. Please retry in a moment.",
    forbidden: "Partner administration requires partnerships access.",
  });
  return (
    <Page wide eyebrow="Staff" back={{ href: "/staff", label: "Staff back office" }} title="Partners">
      <p>
        <Link href="/staff/partners/pending">Applications awaiting review</Link>
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
          {data.partners.length ? (
            data.partners.map((partner) => (
              <article key={partner.id}>
                <h2>
                  <Link href={`/staff/partners/${encodeURIComponent(partner.id)}`}>{partner.organization}</Link>
                </h2>
                <p className="note">
                  {partner.type || "Partner"} · Approved {formatDate(partner.approved_at)}
                  {partner.company_code && (
                    <>
                      {" "}
                      · Company code <code>{partner.company_code}</code>
                    </>
                  )}
                </p>
                <p>
                  {partner.workforce_size.toLocaleString()} people ·{" "}
                  {matchTerms(partner.match_rate_bp, partner.match_cap_minor)} · {partner.initiative_count}{" "}
                  {partner.initiative_count === 1 ? "initiative" : "initiatives"} · {partner.user_count} portal{" "}
                  {partner.user_count === 1 ? "user" : "users"}
                </p>
              </article>
            ))
          ) : (
            <p>No partners have been approved yet.</p>
          )}
          {offset > 0 && (
            <button type="button" className="secondary" onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}>
              Previous partners
            </button>
          )}
          {data.next_offset !== null && (
            <button type="button" className="secondary" onClick={() => setOffset(data.next_offset ?? 0)}>
              More partners
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
