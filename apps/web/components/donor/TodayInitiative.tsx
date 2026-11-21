"use client";

import { useEffect, useState } from "react";
import { api, failureMessage, type Schema } from "@/lib/api";
import { formatLongDay } from "@/lib/dates";
import { formatMoney } from "@/lib/money";
import { useApiResource } from "@/lib/useApiResource";

/** A headline metric as the card shows it, e.g. "1,200 people" or "$4,500.00". */
export function metricValue(metric: Schema<"TodayMetric">): string {
  switch (metric.unit) {
    case "USD_CENTS":
      return formatMoney(metric.value);
    case "PEOPLE":
      return `${metric.value.toLocaleString("en-US")} ${metric.value === 1 ? "person" : "people"}`;
    case "INITIATIVES":
      return `${metric.value.toLocaleString("en-US")} ${metric.value === 1 ? "initiative" : "initiatives"}`;
    case "PARTNERS":
      return `${metric.value.toLocaleString("en-US")} ${metric.value === 1 ? "partner" : "partners"}`;
  }
}

function Spend({ initiative }: { initiative: string }) {
  const [spend, setSpend] = useState<Schema<"InitiativeSpend"> | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  async function load() {
    if (spend || loading) return;
    setError("");
    setLoading(true);
    try {
      setSpend(await api<Schema<"InitiativeSpend">>(`/initiatives/${encodeURIComponent(initiative)}/spend`));
    } catch (cause) {
      setError(failureMessage(cause, "Spending could not be loaded right now. Please retry in a moment."));
    } finally {
      setLoading(false);
    }
  }
  return (
    <details onToggle={(event) => event.currentTarget.open && load()}>
      <summary>See every dollar this initiative has spent</summary>
      {spend &&
        (spend.categories.length ? (
          <ul>
            {spend.categories.map((row) => (
              <li key={row.category}>
                {row.category}: {formatMoney(row.amount_minor)}
              </li>
            ))}
            <li>Total: {formatMoney(spend.total_minor)}</li>
          </ul>
        ) : (
          <p>No spending has been recorded yet.</p>
        ))}
      {loading && <p role="status">Loading spending…</p>}
      {error && <p role="alert">{error}</p>}
    </details>
  );
}

/** "Today your $1 funded": the donor's initiative of the day (A-1 to A-3). */
export function TodayInitiative({ onShown }: { onShown?: (distinctShown: number) => void }) {
  const { resource, data } = useApiResource<Schema<"TodayInitiative">>("/donor/me/today-initiative");
  // Showing this card records the exposure, so its running count is ahead of
  // the dashboard's, which was read a moment earlier (A-4).
  const shown = data?.distinct_shown;
  useEffect(() => {
    if (resource.state === "ready" && typeof shown === "number") onShown?.(shown);
  }, [resource.state, shown, onShown]);
  if (resource.state !== "ready" || !data) return null;
  return (
    <article className="metric today" aria-labelledby="today-initiative">
      <div className="todayGrid">
        <div className="todayMain">
          <h2>Today your $1 funded</h2>
          <h3 id="today-initiative" className="initiativeTitle">
            {data.title}
          </h3>
          <p>
            {data.type} · {data.location} · Since {formatLongDay(data.started_on)}
          </p>
        </div>
        {data.metrics.length > 0 && (
          <ul className="todayMetrics">
            {data.metrics.map((metric) => (
              <li key={metric.label}>
                <span className="metricValue">{metricValue(metric)}</span>
                <span>{metric.label}</span>
                {metric.verified_on && (
                  <span className="verified">
                    <svg aria-hidden="true" viewBox="0 0 24 24" width="16" height="16" focusable="false">
                      <path
                        d="M5 12.5l4.5 4.5L19 7.5"
                        fill="none"
                        stroke="currentColor"
                        strokeWidth="2.5"
                        strokeLinecap="round"
                        strokeLinejoin="round"
                      />
                    </svg>
                    Verified {formatLongDay(metric.verified_on)}
                  </span>
                )}
              </li>
            ))}
          </ul>
        )}
      </div>
      <Spend initiative={data.initiative_id} />
      <details>
        <summary>Why this initiative?</summary>
        <p>
          Each day we show you one initiative in turn. This is display only: every gift is unrestricted and supports DAD
          as a whole, and none is designated to this initiative.
        </p>
      </details>
    </article>
  );
}
