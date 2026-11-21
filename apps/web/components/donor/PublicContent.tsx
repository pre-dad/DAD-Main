import Link from "next/link";
import type { Schema } from "@/lib/api";
import { formatDay } from "@/lib/dates";
import { formatMoney } from "@/lib/money";
import { RefreshButton } from "./RefreshButton";

export function ContentUnavailable() {
  return (
    <div className="errorMessage" role="alert">
      <b>Published records are unavailable right now.</b>
      <span>
        This page shows only figures DAD has published, so nothing is shown in their place. Please retry in a moment.
      </span>
      <RefreshButton>Retry records</RefreshButton>
    </div>
  );
}

export function SpendByCategory({ spend }: { spend: Schema<"OverallSpend"> }) {
  if (!spend.categories.length)
    return (
      <p className="callout">
        No spend has been published yet. Category totals appear here once DAD publishes an initiative&apos;s spend.
      </p>
    );
  return (
    <>
      <p className="note">
        Total across all categories: <strong>{formatMoney(spend.total_minor)}</strong>
      </p>
      <div className="metrics">
        {spend.categories.map((item) => (
          <article key={item.category} className="metric">
            <h2>{item.category}</h2>
            <p className="metricValue">{formatMoney(item.amount_minor)}</p>
          </article>
        ))}
      </div>
    </>
  );
}

export function InitiativeList({ initiatives }: { initiatives: Schema<"PublicInitiative">[] }) {
  if (!initiatives.length)
    return (
      <p className="callout">
        Board-approved plans will appear here as they are published. No initiatives have been published yet.
      </p>
    );
  return (
    <div className="metrics">
      {initiatives.map((item) => (
        <article key={item.slug} className="metric">
          <h2 className="initiativeTitle">
            <Link href={`/initiatives/${item.slug}`}>{item.title}</Link>
          </h2>
          <InitiativeFacts initiative={item} />
        </article>
      ))}
    </div>
  );
}

export function InitiativeFacts({ initiative }: { initiative: Schema<"PublicInitiative"> }) {
  // Calendar dates are UTC throughout (lib/dates.ts), so an initiative flips to "Started" at 00:00 UTC.
  const planned = initiative.started_on > new Date().toISOString().slice(0, 10);
  return (
    <>
      <p>{initiative.summary}</p>
      <p>
        {initiative.partner && <>Partner: {initiative.partner}. </>}
        {planned ? "Planned start" : "Started"}:{" "}
        <time dateTime={initiative.started_on}>{formatDay(initiative.started_on)}</time>.
        {initiative.status === "COMPLETE" && " Complete."}
      </p>
      {planned && <p>Planned initiative. Programs open at launch; this is not a recipient enrollment form.</p>}
    </>
  );
}
