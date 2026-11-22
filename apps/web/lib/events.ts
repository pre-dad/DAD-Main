import type { Schema } from "./api";

/** The API pages staff event lists and rosters in fixed steps of this many rows. */
export const EVENT_PAGE_SIZE = 50;

export function eventState(event: Schema<"StaffEvent">): string {
  if (event.cancelled_at) return "Cancelled";
  return event.publish_state === "PUBLISHED" ? "Published" : "Draft";
}
