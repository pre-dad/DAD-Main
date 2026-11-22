import { formatDateTime } from "@/lib/dates";
import { formatMoney } from "@/lib/money";
import type { UiMetricFormat, UiNavigatePath } from "@/lib/ui-catalog";
import { uiNavigateLabels } from "@/lib/ui-catalog-data";

/** The field of the dashboard that carries the layout; a layout never reads itself. */
const SELF = "insights";

const isRecord = (value: unknown): value is Record<string, unknown> =>
  value !== null && typeof value === "object" && !Array.isArray(value);

/** Resolves an RFC 6901 pointer in the dashboard response; undefined when any step is missing. */
export function resolvePointer(root: unknown, pointer: unknown): unknown {
  if (typeof pointer !== "string" || !pointer.startsWith("/")) return undefined;
  const segments = pointer
    .slice(1)
    .split("/")
    .map((segment) => segment.replaceAll("~1", "/").replaceAll("~0", "~"));
  if (segments[0] === SELF) return undefined;
  let current = root;
  for (const segment of segments) {
    if (!segment || !(isRecord(current) && Object.hasOwn(current, segment))) return undefined;
    current = current[segment];
  }
  return current;
}

const ISO_INSTANT = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}/;

/** A scalar as display text, or null when the value is not one (null, object, array). */
export function scalarText(value: unknown): string | null {
  if (typeof value === "string") return ISO_INSTANT.test(value) ? formatDateTime(value) || value : value;
  if (typeof value === "number") return Number.isFinite(value) ? value.toLocaleString("en-US") : null;
  if (typeof value === "boolean") return value ? "Yes" : "No";
  return null;
}

const plural = (n: number, one: string, many: string) => `${n.toLocaleString("en-US")} ${n === 1 ? one : many}`;

/** A whole-number metric in its catalog format, or null for a value that is not an integer. */
export function metricText(value: unknown, format: unknown): string | null {
  if (typeof value !== "number" || !Number.isSafeInteger(value)) return null;
  switch (format as UiMetricFormat) {
    case "integer":
      return value.toLocaleString("en-US");
    case "usd_minor":
      return formatMoney(value);
    case "days":
      return plural(value, "day", "days");
    case "percent":
      return `${value}%`;
    default:
      return null;
  }
}

/** The label for an allowed in-app path; undefined for anything else, so the link is not drawn. */
export function linkLabel(path: unknown): { href: UiNavigatePath; label: string } | undefined {
  if (typeof path !== "string" || !Object.hasOwn(uiNavigateLabels, path)) return undefined;
  const href = path as UiNavigatePath;
  return { href, label: uiNavigateLabels[href] };
}
