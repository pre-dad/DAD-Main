const calendarDay = new Intl.DateTimeFormat("en-US", {
  month: "short",
  day: "numeric",
  year: "numeric",
  timeZone: "UTC",
});

/** A calendar date with no time of day ("2026-09-30"); it must not shift with the reader's zone. */
export function formatDay(isoDate: string): string {
  const date = new Date(`${isoDate.slice(0, 10)}T00:00:00Z`);
  return Number.isNaN(date.getTime()) ? "" : calendarDay.format(date);
}

const longCalendarDay = new Intl.DateTimeFormat("en-US", {
  month: "long",
  day: "numeric",
  year: "numeric",
  timeZone: "UTC",
});
/** Same as formatDay with the month spelled out ("September 30, 2026"), matching the launch dates. */
export function formatLongDay(isoDate: string): string {
  const date = new Date(`${isoDate.slice(0, 10)}T00:00:00Z`);
  return Number.isNaN(date.getTime()) ? "" : longCalendarDay.format(date);
}
/** The reader's local date of an instant, e.g. "Oct 1, 2026". */
export function formatDate(instant: string): string {
  return new Date(instant).toLocaleDateString("en-US", { month: "short", day: "numeric", year: "numeric" });
}

/**
 * An instant with its time and zone, so a deadline is unambiguous, e.g. "Oct 15, 2026, 8:00 AM EDT".
 * It is shown in timeZone when given, such as an event's venue; a zone this browser does not know
 * falls back to the device's, which the zone name still labels.
 */
export function formatDateTime(instant: string, timeZone?: string): string {
  const options: Intl.DateTimeFormatOptions = {
    month: "short",
    day: "numeric",
    year: "numeric",
    hour: "numeric",
    minute: "2-digit",
    timeZoneName: "short",
  };
  try {
    return new Date(instant).toLocaleString("en-US", { ...options, timeZone: timeZone || undefined });
  } catch {
    return new Date(instant).toLocaleString("en-US", options);
  }
}
