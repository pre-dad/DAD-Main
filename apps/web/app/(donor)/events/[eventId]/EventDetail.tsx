"use client";

import { useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { Page } from "@/components/donor/Page";
import { ApiError, api, failureMessage, type Schema } from "@/lib/api";
import { formatDateTime } from "@/lib/dates";
import { resourceMessages, useApiResource } from "@/lib/useApiResource";
import { eventType, placeSummary } from "../../account/events/Events";

type Event = Schema<"EventView">;
type Loaded = { event: Event; signedIn: boolean };

/**
 * One event (E-3). A signed-in donor sees their own place and can reserve, join the waitlist
 * or cancel (E-4, E-9); anyone else sees the public listing, which never includes invite-only
 * events.
 */
export default function EventDetail({ event: id }: { event: string }) {
  const path = encodeURIComponent(id);
  const { resource, data, reload, failed } = useApiResource<Loaded>(`/events/${path}`, {
    load: async (signal) => {
      try {
        return { event: await api<Event>(`/donor/me/events/${path}`, { signal }), signedIn: true };
      } catch (cause) {
        if (!(cause instanceof ApiError) || cause.status !== 401) throw cause;
        return { event: await api<Event>(`/events/${path}`, { signal }), signedIn: false };
      }
    },
  });
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState("");
  const [error, setError] = useState("");
  const messages = resourceMessages(resource, {
    loading: "Loading event…",
    failure: "This event could not be loaded right now. Please retry in a moment.",
    notFound: "This event isn’t available. It may be invite-only, or the link may be wrong.",
  });

  async function act(action: "reserve" | "cancel") {
    if (busy) return;
    setBusy(true);
    setStatus("");
    setError("");
    try {
      const result = await api<Schema<"ReservationResult">>(`/donor/me/events/${path}/${action}`, {
        method: "POST",
        body: "{}",
      });
      setStatus(
        result.status === "RESERVED"
          ? "Your spot is reserved."
          : result.status === "WAITLISTED"
            ? "You’re on the waitlist. We’ll let you know if a spot opens up."
            : "Your place has been released.",
      );
      reload();
    } catch (cause) {
      setError(failureMessage(cause, "That didn’t go through. Please try again."));
    } finally {
      setBusy(false);
    }
  }

  const event = data?.event;
  const started = event ? new Date(event.starts_at).getTime() <= Date.now() : false;
  const opensLater = Boolean(event?.booking_opens_at && new Date(event.booking_opens_at).getTime() > Date.now());
  const canReserve = !opensLater || Boolean(event?.early_booking);
  return (
    <Page eyebrow={event ? eventType(event) : "Event"} title={event?.title ?? "Event"}>
      {event && (
        <>
          {event.cancelled && (
            <p className="callout warn">This event has been cancelled. Every place has been released.</p>
          )}
          <p className="lead">{event.description}</p>
          <p>
            <time dateTime={event.starts_at}>{formatDateTime(event.starts_at, event.time_zone)}</time> –{" "}
            <time dateTime={event.ends_at}>{formatDateTime(event.ends_at, event.time_zone)}</time>
          </p>
          <p>
            {event.venue}
            {event.address && `, ${event.address}`}, {event.city}
          </p>
          {event.invitation_note && <p className="note">{event.invitation_note}</p>}
          {!event.cancelled && <p>{placeSummary(event)}</p>}
          {!event.cancelled && !started && opensLater && event.booking_opens_at && (
            <p className="note">
              {event.early_booking
                ? "As a Team Captain, you can reserve now, before booking opens on "
                : "Booking opens "}
              <time dateTime={event.booking_opens_at}>{formatDateTime(event.booking_opens_at, event.time_zone)}</time>.
            </p>
          )}
          {!event.cancelled && !started && data.signedIn && (
            <p>
              {event.reservation_status ? (
                <button type="button" className="secondary" disabled={busy} onClick={() => act("cancel")}>
                  {event.reservation_status === "RESERVED" ? "Cancel my spot" : "Leave the waitlist"}
                </button>
              ) : (
                canReserve && (
                  <button type="button" className="button" disabled={busy} onClick={() => act("reserve")}>
                    {event.spots_left === 0 ? "Join the waitlist" : "Reserve a spot"}
                  </button>
                )
              )}
            </p>
          )}
          {!event.cancelled && !data.signedIn && (
            <p>
              <a className="button" href="/account/sign-in">
                Sign in to reserve a spot
              </a>
            </p>
          )}
          <p className="links">
            {!event.cancelled && (event.type !== "INVITE_ONLY" || data.signedIn) && (
              <a href={`/api/${event.type === "INVITE_ONLY" ? "donor/me/" : ""}events/${path}/calendar.ics`} download>
                Add to calendar
              </a>
            )}
            {event.report_issue_url && <a href={event.report_issue_url}>Report an issue</a>}
            {data.signedIn && <a href="/account/events">All events near you</a>}
          </p>
        </>
      )}
      {failed && (
        <button type="button" className="secondary" onClick={reload}>
          Retry event
        </button>
      )}
      <Announcer status={status || messages.status} alert={error || messages.alert} />
    </Page>
  );
}
