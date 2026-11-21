"use client";
import Link from "next/link";
import { useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { KeysetPager } from "@/components/donor/KeysetPager";
import { Page } from "@/components/donor/Page";
import type { Schema } from "@/lib/api";
import { formatDate } from "@/lib/dates";
import { formatMoney } from "@/lib/money";
import { resourceMessages } from "@/lib/useApiResource";
import { useKeysetResource } from "@/lib/useKeysetResource";

const filters = [
  { value: "all", label: "All unresolved" },
  { value: "in_grace", label: "In grace" },
  { value: "lapsed", label: "Grace ended" },
] as const;
type Filter = (typeof filters)[number]["value"];

export default function FailedPayments() {
  const [state, setState] = useState<Filter>("all");
  return (
    <Page wide eyebrow="Staff" back={{ href: "/staff", label: "Staff back office" }} title="Failed payments">
      <p>
        Renewals that failed and have not been paid. A donor keeps their streak for seven days after a failure (grace);
        after that the streak has lapsed.
      </p>
      <label>
        Show
        <select value={state} onChange={(event) => setState(event.target.value as Filter)}>
          {filters.map((filter) => (
            <option key={filter.value} value={filter.value}>
              {filter.label}
            </option>
          ))}
        </select>
      </label>
      <FailedPaymentList key={state} state={state} />
    </Page>
  );
}

function FailedPaymentList({ state }: { state: Filter }) {
  const { resource, data, reload, heading, newer, older } = useKeysetResource<Schema<"FailedPaymentPage">>((cursor) => {
    const query = new URLSearchParams({ state });
    if (cursor) query.set("cursor", cursor);
    return `/donor/staff/failed-payments?${query}`;
  });
  const messages = resourceMessages(resource, {
    loading: "Loading failed payments…",
    failure: "Failed payments could not be loaded right now. Please retry in a moment.",
    forbidden: "Failed payments require donors and money access.",
  });
  return (
    <section aria-labelledby="failed-payment-results">
      <h2 id="failed-payment-results" ref={heading} tabIndex={-1}>
        Unresolved renewals
      </h2>
      {resource.state === "signedOut" && (
        <p>
          <Link className="button" href="/staff/sign-in">
            Staff sign-in
          </Link>
        </p>
      )}
      {data &&
        (data.payments.length ? (
          <>
            {/* Keyboard focus is required for horizontal scrolling in Safari. */}
            {/* biome-ignore lint/a11y/noNoninteractiveTabindex: this is an interactive scroll region. */}
            <section className="tableScroll" tabIndex={0} aria-label="Failed payments table">
              <table className="dataTable">
                <thead>
                  <tr>
                    <th scope="col">Donor</th>
                    <th scope="col">Status</th>
                    <th scope="col">Failed on</th>
                    <th scope="col">Grace ends</th>
                    <th scope="col" className="numeric">
                      Amount
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {data.payments.map((payment) => (
                    <tr key={payment.period_id}>
                      <th scope="row">
                        <Link href={`/staff/donors/${encodeURIComponent(payment.donor_id)}`}>
                          {payment.public_name || payment.email}
                        </Link>
                        {payment.public_name && <div className="note">{payment.email}</div>}
                        {payment.disabled && <div className="note">Account disabled</div>}
                      </th>
                      <td>{payment.state === "in_grace" ? "In grace" : "Grace ended"}</td>
                      <td>
                        <time dateTime={payment.failed_at}>{formatDate(payment.failed_at)}</time>
                      </td>
                      <td>
                        <time dateTime={payment.grace_ends_at}>{formatDate(payment.grace_ends_at)}</time>
                      </td>
                      <td className="numeric">
                        {payment.amount_minor === null ? "Not recorded" : formatMoney(payment.amount_minor)}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </section>
          </>
        ) : (
          <p>No failed payments{state === "all" ? "" : " in this view"}.</p>
        ))}
      {data && (
        <KeysetPager newer={newer} older={older} labels={{ newer: "Newer failures", older: "Older failures" }} />
      )}
      <Announcer status={messages.status} alert={messages.alert} />
      {resource.state !== "signedOut" && (
        <p className="actions">
          {(resource.state === "offline" || resource.state === "error") && (
            <button type="button" className="secondary" onClick={reload}>
              Retry loading
            </button>
          )}
          <button type="button" className="secondary" onClick={reload}>
            Refresh failed payments
          </button>
        </p>
      )}
    </section>
  );
}
