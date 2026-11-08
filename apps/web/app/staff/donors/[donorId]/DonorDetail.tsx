"use client";
import Link from "next/link";
import { useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { Page } from "@/components/donor/Page";
import { api, type Schema } from "@/lib/api";
import { formatDate, formatDateTime } from "@/lib/dates";
import { formatMoney } from "@/lib/money";
import { resourceMessages, useApiResource } from "@/lib/useApiResource";

import { RecomputeRecognition } from "./RecomputeRecognition";

const statusWords: Record<string, string> = {
  active: "Active",
  past_due: "Payment overdue",
  unpaid: "Payment overdue",
  canceled: "Cancelled",
  incomplete_expired: "Cancelled",
  incomplete: "Setting up",
};

export default function DonorDetail({ donor }: { donor: string }) {
  const id = encodeURIComponent(donor);
  const [gifts, setGifts] = useState(0);
  const [referrals, setReferrals] = useState(0);
  const [repairStatus, setRepairStatus] = useState("");
  const path = `/donor/staff/donors/${id}?gifts_offset=${gifts}&referrals_offset=${referrals}`;
  const { resource, data, reload } = useApiResource(path, {
    load: async (signal) => {
      const [detail, actor] = await Promise.all([
        api<Schema<"DonorDetail">>(path, { signal }),
        api<Schema<"StaffView">>("/donor/staff/me", { signal }),
      ]);
      return { detail, actor };
    },
  });
  const messages = resourceMessages(resource, {
    loading: "Loading donor record…",
    failure: "The donor record could not be loaded right now. Please retry in a moment.",
    forbidden: "Your staff role does not permit viewing donor records.",
    notFound: "No donor record matches this link. Search again from the staff back office.",
  });
  const detail = data?.detail;
  const can = (permission: string) => data?.actor.permissions.includes(permission) ?? false;
  return (
    <Page
      wide
      eyebrow="Staff"
      back={{ href: "/staff", label: "Staff back office" }}
      title={detail?.email ?? "Donor record"}
    >
      {resource.state === "signedOut" && (
        <p>
          <Link className="button" href="/staff/sign-in">
            Staff sign-in
          </Link>
        </p>
      )}
      {detail && (
        <>
          <p>
            <Link href={`/staff/messages?donor=${id}`}>Delivery history and resends</Link>
          </p>
          <p>
            Public nickname: {detail.public_name || "Not shared"}
            {` · ${detail.claimed ? "Claimed account" : "Guest (unclaimed)"}`}
            {detail.founding_donor && " · Founding Donor"}
          </p>
          <p>
            Streak: {detail.streak?.current ?? "Updating"} · Best: {detail.streak?.best ?? "Updating"}
            {detail.streak?.at_risk ? " · At risk" : ""}
          </p>
          {detail.streak && <p>Computed {formatDateTime(detail.streak.computed_at)}</p>}
          {can("donor.recompute") && (
            <RecomputeRecognition
              announce={false}
              donor={id}
              onComplete={(message) => {
                setRepairStatus(
                  message ?? "Recognition recomputed from recorded facts. The reason and result are in the audit log.",
                );
                reload();
              }}
            />
          )}
          <p>
            Referrer:{" "}
            {detail.referrer ? (
              <>
                <Link href={`/staff/donors/${encodeURIComponent(detail.referrer.id)}`}>{detail.referrer.email}</Link> ·{" "}
                {detail.referrer.type}
              </>
            ) : (
              "No attributed referrer"
            )}
          </p>
          <h2>Gifts</h2>
          <p>
            {detail.subscription ? (
              <>
                Monthly gift: {formatMoney(detail.subscription.amount_minor)}/month{" · "}
                {statusWords[detail.subscription.status] ?? detail.subscription.status} ·{" "}
                {detail.subscription.stops_at
                  ? `Ends ${formatDate(detail.subscription.stops_at)}`
                  : `Next renewal ${formatDate(detail.subscription.next_renewal)}`}
                {can("gift.cancel") && !detail.subscription.stops_at && (
                  <>
                    {" "}
                    · <Link href={`/staff/finance?donor=${id}`}>Cancel monthly giving</Link>
                  </>
                )}
              </>
            ) : (
              "No active monthly gift."
            )}
          </p>
          {detail.subscription && (
            <p className="note">
              Last updated from Stripe{" "}
              <time dateTime={detail.subscription.observed_at}>{formatDateTime(detail.subscription.observed_at)}</time>.
            </p>
          )}
          {detail.gifts.length ? (
            <ul>
              {detail.gifts.map((gift) => (
                <li key={gift.id}>
                  {formatMoney(gift.amount_minor, gift.currency)} · {gift.frequency} · {formatDateTime(gift.paid_at)} ·{" "}
                  <code>{gift.id}</code>
                  {gift.refunded_minor ? <> · Refunded {formatMoney(gift.refunded_minor, gift.currency)}</> : null}
                  {can("gift.refund") && gift.net_minor > 0 && (
                    <>
                      {" "}
                      · <Link href={`/staff/finance?donor=${id}&gift=${encodeURIComponent(gift.id)}`}>Refund gift</Link>
                    </>
                  )}
                </li>
              ))}
            </ul>
          ) : (
            <p>No gifts recorded.</p>
          )}
          {gifts > 0 && (
            <button type="button" className="secondary" onClick={() => setGifts(0)}>
              Newest gifts
            </button>
          )}
          {detail.next_gifts_offset !== null && (
            <button type="button" className="secondary" onClick={() => setGifts(detail.next_gifts_offset ?? 0)}>
              Older gifts
            </button>
          )}
          <h2>People referred</h2>
          {detail.referred.length ? (
            <ul>
              {detail.referred.map((person) => (
                <li key={person.id}>
                  <Link href={`/staff/donors/${encodeURIComponent(person.id)}`}>{person.email}</Link> · {person.type} ·
                  Net {person.net_minor === null ? "Updating" : formatMoney(person.net_minor)}
                </li>
              ))}
            </ul>
          ) : (
            <p>No converting referrals recorded.</p>
          )}
          {referrals > 0 && (
            <button type="button" className="secondary" onClick={() => setReferrals(0)}>
              First referrals
            </button>
          )}
          {detail.next_referrals_offset !== null && (
            <button type="button" className="secondary" onClick={() => setReferrals(detail.next_referrals_offset ?? 0)}>
              More referrals
            </button>
          )}
          <ActionHistory donor={id} />
        </>
      )}
      <Announcer status={repairStatus || messages.status} alert={messages.alert} />
      {(resource.state === "offline" || resource.state === "error") && (
        <button type="button" className="secondary" onClick={reload}>
          Retry loading
        </button>
      )}
    </Page>
  );
}

function ActionHistory({ donor }: { donor: string }) {
  // Keyset paging: each entry is the cursor that opened a page, the first being none.
  const [cursors, setCursors] = useState<string[]>([]);
  const cursor = cursors.at(-1);
  const { resource, data, reload } = useApiResource<Schema<"ActionPage">>(
    `/donor/staff/donors/${donor}/actions${cursor ? `?cursor=${encodeURIComponent(cursor)}` : ""}`,
  );
  const messages = resourceMessages(resource, {
    loading: "Loading staff actions…",
    failure: "Staff actions could not be loaded right now. Please retry in a moment.",
  });
  return (
    <section aria-labelledby="staff-actions">
      <h2 id="staff-actions">Staff financial actions</h2>
      {data && (
        <>
          {data.actions.length ? (
            <ul>
              {data.actions.map((item) => (
                <li key={item.id}>
                  {item.kind} · {item.status}
                  {item.amount_minor !== null && ` · ${formatMoney(item.amount_minor)}`}
                  {item.period_end && ` · Stops ${formatDate(item.period_end)}`} ·{" "}
                  {item.completed_at ? `Completed ${formatDateTime(item.completed_at)}` : "Not completed"}
                  <p>
                    Reason: {item.reason}. Staff member: <code>{item.actor_id}</code>. Target:{" "}
                    <code>{item.target_id}</code>
                  </p>
                </li>
              ))}
            </ul>
          ) : (
            <p>No refunds or cancellations recorded.</p>
          )}
          {cursors.length > 0 && (
            <button type="button" className="secondary" onClick={() => setCursors(cursors.slice(0, -1))}>
              Newer actions
            </button>
          )}
          {data.next_cursor !== null && (
            <button
              type="button"
              className="secondary"
              onClick={() => setCursors([...cursors, data.next_cursor ?? ""])}
            >
              Older actions
            </button>
          )}
        </>
      )}
      <Announcer status={messages.status} alert={messages.alert} />
      {(resource.state === "offline" || resource.state === "error") && (
        <button type="button" className="secondary" onClick={reload}>
          Retry staff actions
        </button>
      )}
    </section>
  );
}
