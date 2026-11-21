const localPattern = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::(\d{2}))?$/;

function formatter(zone: string) {
  return new Intl.DateTimeFormat("en-US", {
    timeZone: zone,
    hourCycle: "h23",
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

function wallClock(ms: number, zone: string): number {
  const parts = Object.fromEntries(
    formatter(zone)
      .formatToParts(new Date(ms))
      .map((part) => [part.type, Number(part.value)]),
  );
  return Date.UTC(parts.year, parts.month - 1, parts.day, parts.hour, parts.minute, parts.second);
}

/** Whether `zone` is an IANA zone name this runtime resolves. */
export function validZone(zone: string): boolean {
  try {
    formatter(zone);
    return true;
  } catch {
    return false;
  }
}

/**
 * A datetime-local value (the wall clock in `zone`) as an RFC 3339 instant, or
 * null when it is not a time or the zone is unknown. A wall time inside a
 * spring-forward gap resolves to the instant just after the gap.
 */
export function zonedInstant(local: string, zone: string): string | null {
  const match = localPattern.exec(local);
  if (!match || !validZone(zone)) return null;
  const [year, month, day, hour, minute, second] = match.slice(1).map((part) => Number(part ?? 0));
  const wall = Date.UTC(year, month - 1, day, hour, minute, second);
  const first = wall - (wallClock(wall, zone) - wall);
  const resolved = wall - (wallClock(first, zone) - first);
  const date = new Date(resolved);
  return Number.isNaN(date.getTime()) ? null : date.toISOString().replace(/\.\d{3}Z$/, "Z");
}

/** An instant as a datetime-local value showing the wall clock in `zone`. */
export function zonedLocal(instant: string, zone: string): string {
  const shifted = new Date(wallClock(new Date(instant).getTime(), zone));
  return shifted.toISOString().slice(0, 16);
}
