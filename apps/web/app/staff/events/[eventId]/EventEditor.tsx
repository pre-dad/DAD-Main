"use client";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { type FormEvent, useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { Page } from "@/components/donor/Page";
import { api, failureMessage, type Schema } from "@/lib/api";
import { formatDateTime } from "@/lib/dates";
import { resourceMessages, useApiResource } from "@/lib/useApiResource";
import { eventState } from "../Events";

type Detail = Schema<"StaffEventDetail">;
type Input = Schema<"EventInput">;
type Perk = NonNullable<Input["qualifying_perk"]>;

const perks: Record<Perk, string> = {
  "": "Guest list only",
  FOUNDING_EVENTS: "Founding Donor badge",
  MILESTONE_EVENTS: "Milestone badge",
  TEAM_CAPTAIN: "Team Captain badge",
};

/** An instant as a datetime-local value in the browser's zone. */
export function localInput(instant: string): string {
  const date = new Date(instant);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

/** A datetime-local value, read in the browser's zone, as an RFC 3339 instant. */
export function instant(local: string): string | null {
  const date = new Date(local);
  return Number.isNaN(date.getTime()) ? null : date.toISOString().replace(/\.\d{3}Z$/, "Z");
}

/** Zones offered as suggestions; any IANA name the server accepts can be typed. */
const zones = [
  "America/New_York",
  "America/Chicago",
  "America/Denver",
  "America/Phoenix",
  "America/Los_Angeles",
  "America/Anchorage",
  "Pacific/Honolulu",
];

const field = (form: FormData, name: string) => String(form.get(name) ?? "").trim();

/** The event fields from the editor form, or an error to show. */
export function eventInput(form: FormData): Input | string {
  const starts = instant(field(form, "starts_at"));
  const ends = instant(field(form, "ends_at"));
  if (!starts || !ends) return "Enter when the event starts and ends.";
  if (ends <= starts) return "The event must end after it starts.";
  const capacity = field(form, "capacity");
  if (!/^\d+$/.test(capacity)) return "Enter the capacity as a whole number, or 0 for no limit.";
  const opensField = field(form, "booking_opens_at");
  const opens = opensField ? instant(opensField) : "";
  if (opens === null) return "Enter when booking opens, or leave it empty.";
  if (opens && opens >= starts) return "Booking must open before the event starts.";
  const type = field(form, "type") as Input["type"];
  return {
    slug: field(form, "slug"),
    title: field(form, "title"),
    type,
    description: field(form, "description"),
    venue: field(form, "venue"),
    address: field(form, "address"),
    city: field(form, "city"),
    zip: field(form, "zip"),
    time_zone: field(form, "time_zone"),
    starts_at: starts,
    ends_at: ends,
    capacity: Number(capacity),
    qualifying_perk: type === "INVITE_ONLY" ? (field(form, "perk") as Perk) : "",
    booking_opens_at: opens,
  };
}

export default function EventEditor({ event }: { event: string | null }) {
  const router = useRouter();
  const id = event && encodeURIComponent(event);
  const [offset, setOffset] = useState(0);
  const { resource, data, reload } = useApiResource<Detail>(id && `/donor/staff/events/${id}?offset=${offset}`);
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState("");
  const [error, setError] = useState("");
  const [qr, setQr] = useState(false);
  const messages = id
    ? resourceMessages(resource, {
        loading: "Loading event…",
        failure: "This event could not be loaded right now. Please retry in a moment.",
        forbidden: "Events require Growth access.",
        notFound: "No event matches this link.",
      })
    : { status: "", alert: "" };

  async function run(done: string, action: () => Promise<unknown>) {
    if (busy) return;
    setBusy(true);
    setStatus("");
    setError("");
    try {
      await action();
      setStatus(done);
      if (id) reload();
    } catch (cause) {
      setError(failureMessage(cause, "That change could not be saved. Nothing was changed."));
    } finally {
      setBusy(false);
    }
  }

  const post = (path: string, body?: unknown) =>
    api<{ id?: string }>(`/donor/staff/events${path}`, {
      method: "POST",
      body: body === undefined ? undefined : JSON.stringify(body),
    });

  function save(submitted: FormEvent<HTMLFormElement>) {
    submitted.preventDefault();
    const input = eventInput(new FormData(submitted.currentTarget));
    if (typeof input === "string") {
      setError(input);
      return;
    }
    if (id) {
      void run("Event saved.", () => post(`/${id}`, input));
      return;
    }
    void run("Draft event created.", async () => {
      const created = await post("", input);
      if (created.id) router.push(`/staff/events/${encodeURIComponent(created.id)}`);
    });
  }

  function cancelEvent(detail: Detail) {
    const holders = detail.reserved + detail.waitlisted;
    if (!window.confirm(`Cancel ${detail.title}? ${holders} ${holders === 1 ? "person" : "people"} will be notified.`))
      return;
    void run("Event cancelled. Everyone holding a place is being notified.", () => post(`/${id}/cancel`));
  }

  const detail = data;
  const cancelled = Boolean(detail?.cancelled_at);
  return (
    <Page
      wide
      eyebrow="Staff"
      back={{ href: "/staff/events", label: "Events" }}
      title={detail?.title ?? (id ? "Event" : "New event")}
    >
      {resource.state === "signedOut" && id && (
        <p>
          <Link className="button" href="/staff/sign-in">
            Staff sign-in
          </Link>
        </p>
      )}
      {(!id || detail) && (
        <>
          {detail && (
            <p className="note">
              {eventState(detail)}
              {detail.cancelled_at && ` ${formatDateTime(detail.cancelled_at)}`}
            </p>
          )}
          <form
            onSubmit={save}
            key={
              detail
                ? `${detail.id}-${detail.starts_at}-${detail.capacity}-${detail.booking_opens_at}-${detail.time_zone}`
                : "new"
            }
          >
            <fieldset disabled={cancelled}>
              <label>
                Slug
                <input name="slug" defaultValue={detail?.slug} readOnly={Boolean(detail)} maxLength={100} required />
              </label>
              <label>
                Title
                <input name="title" defaultValue={detail?.title} maxLength={150} required />
              </label>
              <label>
                Type
                <select name="type" defaultValue={detail?.type ?? "VOLUNTEER"}>
                  <option value="VOLUNTEER">Volunteer</option>
                  <option value="COMMUNITY">Community</option>
                  <option value="INVITE_ONLY">Invite only</option>
                </select>
              </label>
              <label>
                Invite-only access
                <select name="perk" defaultValue={detail?.qualifying_perk ?? ""}>
                  {Object.entries(perks).map(([perk, label]) => (
                    <option key={perk} value={perk}>
                      {label}
                    </option>
                  ))}
                </select>
              </label>
              <label>
                Description
                <textarea name="description" defaultValue={detail?.description} maxLength={4000} />
              </label>
              <label>
                Venue
                <input name="venue" defaultValue={detail?.venue} maxLength={200} required />
              </label>
              <label>
                Address
                <input name="address" defaultValue={detail?.address} maxLength={300} />
              </label>
              <label>
                City
                <input name="city" defaultValue={detail?.city} maxLength={80} required />
              </label>
              <label>
                ZIP
                <input name="zip" defaultValue={detail?.zip} inputMode="numeric" maxLength={10} />
              </label>
              <label>
                Time zone
                <input
                  name="time_zone"
                  defaultValue={detail?.time_zone}
                  list="event-time-zones"
                  maxLength={64}
                  aria-describedby="time-zone-help"
                />
              </label>
              <datalist id="event-time-zones">
                {zones.map((zone) => (
                  <option key={zone} value={zone} />
                ))}
              </datalist>
              <p className="note" id="time-zone-help">
                The venue's IANA zone, such as America/Chicago. Donors see the event time in it. Leave empty to use the
                zone of the ZIP.
              </p>
              <label>
                Starts
                <input
                  name="starts_at"
                  type="datetime-local"
                  defaultValue={detail ? localInput(detail.starts_at) : undefined}
                  required
                />
              </label>
              <label>
                Ends
                <input
                  name="ends_at"
                  type="datetime-local"
                  defaultValue={detail ? localInput(detail.ends_at) : undefined}
                  required
                />
              </label>
              <label>
                Booking opens (optional)
                <input
                  name="booking_opens_at"
                  type="datetime-local"
                  defaultValue={detail?.booking_opens_at ? localInput(detail.booking_opens_at) : undefined}
                  aria-describedby="booking-opens-help"
                />
              </label>
              <p className="note" id="booking-opens-help">
                Leave empty to open booking when the event is published. Team Captains can reserve 48 hours before
                booking opens.
              </p>
              <label>
                Capacity (0 for no limit)
                <input name="capacity" inputMode="numeric" defaultValue={detail?.capacity ?? 0} required />
              </label>
              <button className="button" type="submit" disabled={busy}>
                {detail ? "Save event" : "Create draft"}
              </button>
            </fieldset>
          </form>
          {detail && !cancelled && (
            <p>
              {detail.publish_state === "DRAFT" && (
                <button
                  type="button"
                  className="button"
                  disabled={busy}
                  onClick={() => run("Event published.", () => post(`/${id}/publish`))}
                >
                  Publish event
                </button>
              )}{" "}
              <button type="button" className="secondary" disabled={busy} onClick={() => cancelEvent(detail)}>
                Cancel event
              </button>
            </p>
          )}
          {detail && detail.publish_state === "PUBLISHED" && !cancelled && (
            <section aria-labelledby="check-in-qr">
              <h2 id="check-in-qr">Check-in QR code</h2>
              {qr ? (
                // biome-ignore lint/performance/noImgElement: an API-rendered SVG behind the staff session, not an optimizable asset
                <img
                  src={`/api/donor/staff/events/${id}/check-in.svg`}
                  alt={`Check-in QR code for ${detail.title}`}
                  width={320}
                  height={320}
                />
              ) : (
                <button type="button" className="secondary" onClick={() => setQr(true)}>
                  Show check-in QR code
                </button>
              )}
            </section>
          )}
          {detail && (
            <>
              <h2>RSVPs</h2>
              <p>
                {detail.reserved}
                {detail.capacity > 0 ? ` of ${detail.capacity}` : ""} reserved · {detail.waitlisted} waitlisted ·{" "}
                {detail.checked_in} checked in
              </p>
              {detail.attendees.length ? (
                <ol>
                  {detail.attendees.map((attendee) => (
                    <li key={attendee.donor_id}>
                      <Link href={`/staff/donors/${encodeURIComponent(attendee.donor_id)}`}>{attendee.email}</Link>
                      {attendee.public_name && ` (${attendee.public_name})`} ·{" "}
                      {attendee.status === "RESERVED" ? "Reserved" : "Waitlisted"}{" "}
                      {formatDateTime(attendee.reserved_at)}
                      {attendee.checked_in_at
                        ? ` · Checked in ${formatDateTime(attendee.checked_in_at)}`
                        : attendee.status === "RESERVED" && (
                            <>
                              {" "}
                              <button
                                type="button"
                                className="link"
                                disabled={busy}
                                onClick={() =>
                                  run(`${attendee.email} checked in.`, () =>
                                    post(`/${id}/attendees/${encodeURIComponent(attendee.donor_id)}/check-in`),
                                  )
                                }
                              >
                                Check in {attendee.email}
                              </button>
                            </>
                          )}
                    </li>
                  ))}
                </ol>
              ) : (
                <p>{offset > 0 ? "No RSVPs on this page." : "No one has reserved yet."}</p>
              )}
              <nav aria-label="RSVP pages">
                {offset > 0 && (
                  <button type="button" className="secondary" onClick={() => setOffset(Math.max(0, offset - 50))}>
                    Previous RSVPs
                  </button>
                )}
                {detail.next_offset != null && (
                  <button type="button" className="secondary" onClick={() => setOffset(detail.next_offset ?? 0)}>
                    Next RSVPs
                  </button>
                )}
              </nav>
            </>
          )}
        </>
      )}
      <Announcer status={status || messages.status} alert={error || messages.alert} />
      {(resource.state === "offline" || resource.state === "error") && id && (
        <button type="button" className="secondary" onClick={reload}>
          Retry loading
        </button>
      )}
    </Page>
  );
}
