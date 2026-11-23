"use client";

import Link from "next/link";
import { useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { Page } from "@/components/donor/Page";
import { SignInPrompt } from "@/components/donor/SignInPrompt";
import { TodayInitiative } from "@/components/donor/TodayInitiative";
import { Insights } from "@/components/insights/Insights";
import { canRender } from "@/components/insights/panel";
import type { Schema } from "@/lib/api";
import { formatDateTime } from "@/lib/dates";
import { foundingDaysLeft, foundingDeadlineLabel, launchDateLabel } from "@/lib/launch";
import { formatMoney } from "@/lib/money";
import { resourceMessages, useApiResource } from "@/lib/useApiResource";

/** "Near a milestone" means this many days or fewer to go. */
const MILESTONE_NEAR_DAYS = 7;
const streakDayKeys = Array.from({ length: 30 }, (_, day) => `streak-day-${day + 1}`);

/** One figure in the compact totals strip. Loading and loaded share it so they cannot drift apart. */
function Stat({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="stat">
      <dt>{label}</dt>
      <dd>{children}</dd>
    </div>
  );
}
function LoadingMetric({
  title,
  children,
  detail = false,
  link,
  value,
}: {
  title: string;
  children: React.ReactNode;
  detail?: boolean;
  link?: string;
  /** Text the real value will wrap like, for a value long enough to take two lines. */
  value?: string;
}) {
  return (
    <article className="metric">
      <h2>{title}</h2>
      <p className="metricValue">
        {value ? <span className="metricSkeletonCopy">{value}</span> : <span className="metricSkeleton" />}
      </p>
      <p>{children}</p>
      {link && <span className="metricLink">{link}</span>}
      {detail && (
        <p className="metricSkeletonDetail">
          <span>View Link and IRL breakdown</span>
          <span>+</span>
        </p>
      )}
    </article>
  );
}
/** A referral count of zero is shown as quiet text; null (unknown) never reaches this. */
function ReferralMetric({
  title,
  count,
  description,
  empty,
  href,
  linkLabel,
}: {
  title: string;
  count: number | undefined;
  description: string;
  empty: string;
  href: string;
  linkLabel: string;
}) {
  return (
    <article className="metric">
      <h2>{title}</h2>
      <p className={count === 0 ? "metricValue zero" : "metricValue"}>{count ?? "Updating"}</p>
      <p>{count === 0 ? empty : description}</p>
      <Link className="metricLink" href={href}>
        {linkLabel} <span aria-hidden="true">→</span>
      </Link>
    </article>
  );
}
function StarIcon() {
  return (
    <svg
      className="earnedStar"
      aria-hidden="true"
      viewBox="0 0 24 24"
      width="20"
      height="20"
      fill="currentColor"
      focusable="false"
    >
      <path d="M12 2.5l2.9 6.1 6.6.9-4.8 4.6 1.2 6.6L12 17.5l-5.9 3.2 1.2-6.6L2.5 9.5l6.6-.9z" />
    </svg>
  );
}
const count = (value: number) => value.toLocaleString("en-US");
const milestoneCopy = (daysToNext: number, milestone: number) =>
  `${count(daysToNext)} ${daysToNext === 1 ? "day" : "days"} to your ${count(milestone)}-day milestone.`;

/** The single most useful next step, in priority order; the at-risk callout below carries the detail. */
function nextStep(data: Schema<"DonorDashboard">): string {
  if (data.state !== "ready") return "";
  if (data.streak?.at_risk) return "Your latest monthly payment didn’t go through.";
  if (data.referrals && data.referrals.link === 0 && data.referrals.irl === 0)
    return "Your first invite is one tap away.";
  const ladder = data.streak?.ladder;
  if (ladder?.next_milestone && ladder.days_to_next && ladder.days_to_next <= MILESTONE_NEAR_DAYS)
    return milestoneCopy(ladder.days_to_next, ladder.next_milestone);
  return "Here is what your giving has done so far.";
}

export default function Dashboard() {
  const { resource, data, failed, reload } = useApiResource<Schema<"DonorDashboard">>("/donor/me/dashboard");
  const [shownInitiatives, setShownInitiatives] = useState(0);
  const messages = resourceMessages(resource, {
    loading: "Loading your dashboard…",
    failure: "Your dashboard could not be loaded right now. Please retry in a moment.",
  });
  const updating =
    data?.state === "preparing"
      ? "We’re preparing your dashboard. Please refresh shortly."
      : data?.state === "ready" && !data.referrals
        ? "Referral totals are updating. Please refresh shortly."
        : data?.state === "ready" && !data.streak
          ? "Your streak is updating. Please refresh shortly."
          : "";
  const atRisk = data?.state === "ready" && data.streak?.at_risk;
  // Computed in the browser (this component renders only after its data arrives), so the count is
  // always today's New York date and never baked into cached HTML.
  const foundingLeft = data?.state === "ready" && !data.founding_donor ? foundingDaysLeft(new Date()) : null;
  return (
    <Page wide eyebrow="Your impact" title={resource.state === "signedOut" ? "Your dashboard" : "Welcome back."}>
      {resource.state !== "signedOut" && (
        <div className="dashBand">
          {/* Reserved while loading so the band is the same height before and after the data arrives. */}
          <p className="dashSub">{data ? nextStep(data) : ""}</p>
          <div className="actions">
            {/* The at-risk callout owns the page's one primary button. */}
            <Link className={atRisk ? "secondary" : "button"} href="/account/share">
              Invite someone
            </Link>
            <Link className="secondary" href="/account/code">
              Show my code
            </Link>
          </div>
        </div>
      )}
      <p className="note">
        DAD launches {launchDateLabel()}. This preview shows test payments and simulated recognition only.
      </p>
      {resource.state === "signedOut" ? (
        <SignInPrompt>Sign in to view your dashboard</SignInPrompt>
      ) : (
        <>
          {(resource.state === "loading" || data?.state === "preparing") && (
            <>
              <div aria-hidden="true">
                <dl className="statStrip">
                  <Stat label="Your giving">
                    <span className="metricSkeleton" />
                  </Stat>
                  <Stat label="Initiatives funded">
                    <span className="metricSkeleton" />
                  </Stat>
                  <Stat label="Events volunteered">
                    <span className="metricSkeleton" />
                  </Stat>
                </dl>
              </div>
              <div className="metrics pair" aria-hidden="true">
                <LoadingMetric title="Link Referrals" link="Share your link →">
                  People who gave after opening your link.
                </LoadingMetric>
                <LoadingMetric title="IRL Referrals" link="Show my code →">
                  People who gave after scanning your code.
                </LoadingMetric>
              </div>
              <div className="metrics" aria-hidden="true">
                <LoadingMetric title="Money raised because of you" detail>
                  Collected test gifts after successful refunds.
                </LoadingMetric>
                <LoadingMetric title="Giving streak">
                  Consecutive days your monthly gift has stayed current.
                </LoadingMetric>
                <LoadingMetric title="Recognition" value="Earned. Yours for good.">
                  <span className="metricSkeletonCopy">
                    You joined before DAD launched. Thank you for starting this with us.
                  </span>
                </LoadingMetric>
              </div>
            </>
          )}
          {data?.state === "ready" && (
            <>
              {data.streak?.at_risk && (
                <div className="callout warn">
                  <p>
                    Your streak is at risk: your latest monthly payment didn’t go through.
                    {data.streak.grace_ends_at && (
                      <>
                        {" "}
                        Update your card by{" "}
                        <time dateTime={data.streak.grace_ends_at}>{formatDateTime(data.streak.grace_ends_at)}</time>,
                        when the grace period ends, to keep your {data.streak.ladder?.days ?? 0}-day streak.
                      </>
                    )}
                  </p>
                  <Link className="button" href="/account/payment-method">
                    Update payment method
                  </Link>
                </div>
              )}
              <dl className="statStrip">
                <Stat label="Your giving">
                  {data.impact.given_minor === null ? "Updating" : formatMoney(data.impact.given_minor)}
                </Stat>
                <Stat label="Initiatives funded">{Math.max(data.impact.initiatives_funded, shownInitiatives)}</Stat>
                <Stat label="Events volunteered">{data.impact.events_volunteered}</Stat>
              </dl>
              <TodayInitiative onShown={setShownInitiatives} />
              <div className="metrics pair">
                <ReferralMetric
                  title="Link Referrals"
                  count={data.referrals?.link}
                  description="People who gave after opening your link."
                  empty="No one yet. Your link is ready to share."
                  href="/account/share"
                  linkLabel="Share your link"
                />
                <ReferralMetric
                  title="IRL Referrals"
                  count={data.referrals?.irl}
                  description="People who gave after scanning your code."
                  empty="No one yet. Your code is ready to scan."
                  href="/account/code"
                  linkLabel="Show my code"
                />
              </div>
              <div className="metrics">
                <article className="metric">
                  <h2>Money raised because of you</h2>
                  <p className={data.referrals?.total_minor === 0 ? "metricValue zero" : "metricValue"}>
                    {data.referrals ? formatMoney(data.referrals.total_minor) : "Updating"}
                  </p>
                  <p>
                    {data.referrals?.total_minor === 0
                      ? "Gifts from people you invite will add up here."
                      : "Collected test gifts after successful refunds."}
                  </p>
                  {data.referrals && (
                    <details>
                      <summary>View Link and IRL breakdown</summary>
                      <p>Link: {formatMoney(data.referrals.link_minor)}</p>
                      <p>IRL: {formatMoney(data.referrals.irl_minor)}</p>
                    </details>
                  )}
                </article>
                <article className="metric">
                  <h2>Giving streak</h2>
                  <p className="metricValue">
                    {data.streak?.ladder
                      ? `${count(data.streak.ladder.days)} ${data.streak.ladder.days === 1 ? "day" : "days"}`
                      : data.streak
                        ? "Starting today"
                        : "Updating"}
                  </p>
                  {data.streak?.ladder ? (
                    <>
                      <p>
                        {data.streak.ladder.next_milestone && data.streak.ladder.days_to_next
                          ? milestoneCopy(data.streak.ladder.days_to_next, data.streak.ladder.next_milestone)
                          : "Your giving streak keeps growing every day."}
                      </p>
                      <div
                        className="streakBar"
                        role="img"
                        aria-label={`${data.streak.ladder.bar.filter(Boolean).length} of the last 30 days in your current streak`}
                      >
                        {streakDayKeys.map((key, index) => (
                          <span
                            key={key}
                            className={data.streak?.ladder?.bar[index] ? "active" : ""}
                            aria-hidden="true"
                          />
                        ))}
                      </div>
                      {data.rank && (
                        <p>
                          Top {data.rank.national_percentile}% nationally
                          {data.rank.city_rank && data.rank.city
                            ? ` · #${data.rank.city_rank} in ${data.rank.city}`
                            : ""}
                        </p>
                      )}
                    </>
                  ) : data.streak ? (
                    <p>Your day-based streak begins with your current monthly gift.</p>
                  ) : (
                    <p>We’re checking your billing history. Your streak is temporarily unavailable.</p>
                  )}
                </article>
                {data.network && (
                  <article className="metric">
                    <h2>Your network</h2>
                    <p className="metricValue">{formatMoney(data.network.annualized_minor)} a year</p>
                    <p>
                      {data.network.active_count} active {data.network.active_count === 1 ? "donor" : "donors"} you
                      brought in.
                    </p>
                  </article>
                )}
                {data.founding_donor && (
                  <article className="metric earned">
                    <h2>
                      <StarIcon /> Founding Donor
                    </h2>
                    <p className="metricValue">Earned. Yours for good.</p>
                    <p>You joined before DAD launched. Thank you for starting this with us.</p>
                  </article>
                )}
              </div>
              {canRender(data.insights) && (
                <section className="insights" aria-labelledby="insights-heading">
                  <h2 id="insights-heading">Your insights</h2>
                  <Insights spec={data.insights.spec} dashboard={data} />
                </section>
              )}
              {!data.founding_donor && (
                <p className="note">
                  Founding Donor badge: not yet earned.{" "}
                  {foundingLeft === null
                    ? "The Founding window has closed."
                    : `A first gift by ${foundingDeadlineLabel()} (New York time) earns it permanently. ${foundingLeft} ${foundingLeft === 1 ? "day" : "days"} left.`}
                </p>
              )}
            </>
          )}
          <Announcer
            status={messages.status || updating}
            alert={messages.alert}
            statusShownElsewhere={resource.state === "loading"}
          />
          {(failed || updating) && (
            <button type="button" className="secondary" onClick={reload}>
              Refresh dashboard
            </button>
          )}
          <nav aria-label="Account" className="accountNav">
            <div>
              <h2>Your giving</h2>
              <ul>
                <li>
                  <Link href="/account/giving">Gift history and receipts</Link>
                </li>
                <li>
                  <Link href="/account/recurring">Manage monthly giving</Link>
                </li>
                <li>
                  <Link href="/account/payment-method">Payment method</Link>
                </li>
              </ul>
            </div>
            <div>
              <h2>Your circle</h2>
              <ul>
                <li>
                  <Link href="/account/share">Share and invite</Link>
                </li>
                <li>
                  <Link href="/account/network">Your network</Link>
                </li>
                <li>
                  <Link href="/account/badges">Badges</Link>
                </li>
              </ul>
            </div>
            <div>
              <h2>You</h2>
              <ul>
                <li>
                  <Link href="/account/events">Events near you</Link>
                </li>
                <li>
                  <Link href="/account/employer">Employer and team</Link>
                </li>
                <li>
                  <Link href="/account/profile">Profile and notifications</Link>
                </li>
              </ul>
            </div>
          </nav>
        </>
      )}
    </Page>
  );
}
