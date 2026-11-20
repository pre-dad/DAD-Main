"use client";

import Form from "next/form";
import Link from "next/link";
import { Announcer } from "@/components/donor/Announcer";
import { Page } from "@/components/donor/Page";
import { SignInPrompt } from "@/components/donor/SignInPrompt";
import type { Schema } from "@/lib/api";
import { formatDate, formatDateTime } from "@/lib/dates";
import { formatMoney } from "@/lib/money";
import { FIRST_SUMMARY_YEAR } from "@/lib/taxYear";
import { resourceMessages, useApiResource } from "@/lib/useApiResource";

const refundStatuses: Record<string, string> = {
  succeeded: "Completed",
  pending: "Pending",
  requires_action: "Needs action",
  failed: "Failed",
  canceled: "Canceled",
};
const refundStatus = (status: string) => refundStatuses[status] ?? status;

/** What is shown comes from the address (?gift= or ?year=), so every record can be linked, bookmarked and reached with Back. */
export default function Receipts({
  gift,
  year,
  defaultYear,
  latestYear,
}: {
  gift?: string;
  year?: number;
  defaultYear: number;
  latestYear: number;
}) {
  const path = gift
    ? `/donor/me/receipts/${encodeURIComponent(gift)}`
    : year
      ? `/donor/me/annual-summary/${year}`
      : null;
  const {
    resource,
    data: document,
    failed,
    reload,
  } = useApiResource<Schema<"Receipt"> | Schema<"AnnualSummary">>(path);
  const messages = resourceMessages(resource, {
    loading: "Loading gift records…",
    failure: "This record could not be loaded right now. Please try again in a moment.",
    notFound: gift
      ? "There is no gift record at this link. Choose a gift from My giving."
      : "There is no summary for that year. Choose a year in which you gave.",
  });
  const active = path !== null;
  return (
    <Page
      eyebrow="My account"
      title="Gift records and annual summaries"
      back={{ href: "/account/giving", label: "My giving" }}
    >
      <p className="callout">
        <strong>Sandbox only — not a tax receipt. No real donation was collected.</strong>
      </p>
      {resource.state === "signedOut" && active ? (
        <SignInPrompt>Sign in to view your gift records</SignInPrompt>
      ) : (
        <>
          <Form action="/account/receipts">
            <label>
              Calendar year
              <input
                key={year}
                name="year"
                type="number"
                min={FIRST_SUMMARY_YEAR}
                max={latestYear}
                defaultValue={year ?? defaultYear}
                required
              />
            </label>
            <button type="submit" className="button">
              View annual summary
            </button>
          </Form>
          {document && (
            <>
              {"gift" in document ? (
                <>
                  <h2>Test gift record</h2>
                  <ul className="rows">
                    <li>
                      <span>Reference</span>
                      <span className="hash">{document.receipt_id}</span>
                    </li>
                    <li>
                      <span>
                        {document.gift.frequency === "MONTHLY" ? "Monthly gift" : "One-time gift"} ·{" "}
                        {formatDateTime(document.gift.paid_at)}
                      </span>
                      <strong>{formatMoney(document.gift.amount_minor)}</strong>
                    </li>
                    {document.gift.refunds.map((refund) => (
                      <li key={refund.id}>
                        <span>
                          Refund · {refundStatus(refund.status)} · observed {formatDateTime(refund.observed_at)}
                        </span>
                        <span>{formatMoney(refund.amount_minor)}</span>
                      </li>
                    ))}
                    <li>
                      <span>Successful refunds</span>
                      <span>{formatMoney(document.gift.refunded_minor)}</span>
                    </li>
                    <li>
                      <span>Net test amount</span>
                      <strong>{formatMoney(document.gift.net_minor)}</strong>
                    </li>
                  </ul>
                </>
              ) : (
                <>
                  <h2>{document.year} annual test summary</h2>
                  <ul className="rows">
                    <li>
                      <span>Collected</span>
                      <span>{formatMoney(document.amount_minor)}</span>
                    </li>
                    <li>
                      <span>Successful refunds</span>
                      <span>{formatMoney(document.refunded_minor)}</span>
                    </li>
                    <li>
                      <span>Net</span>
                      <strong>{formatMoney(document.net_minor)}</strong>
                    </li>
                  </ul>
                  <h2>Gifts</h2>
                  {document.gifts.length ? (
                    <ul className="rows">
                      {document.gifts.map((item) => (
                        <li key={item.id}>
                          <Link href={`/account/receipts?gift=${encodeURIComponent(item.id)}`}>
                            {formatDate(item.paid_at)} · {formatMoney(item.amount_minor)}
                          </Link>
                          <span>Net {formatMoney(item.net_minor)}</span>
                        </li>
                      ))}
                    </ul>
                  ) : (
                    <p>No confirmed gifts in this year.</p>
                  )}
                  <p className="note">
                    Calendar year uses {document.timezone}. Refunds reflect the latest verified adjustments to these
                    gifts, including adjustments observed in later years. This is not a tax deduction calculation.
                  </p>
                </>
              )}
              <p className="note">
                As of <time dateTime={document.as_of}>{formatDateTime(document.as_of)}</time>.
              </p>
              <button type="button" className="secondary" onClick={() => window.print()}>
                Print test record
              </button>
            </>
          )}
          {active && failed && (
            <button type="button" className="secondary" onClick={reload}>
              Refresh gift records
            </button>
          )}
        </>
      )}
      <Announcer status={active ? messages.status : ""} alert={active ? messages.alert : ""} />
    </Page>
  );
}
