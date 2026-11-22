const formatters = new Map<string, Intl.NumberFormat>();

function formatter(currency: string, fractionDigits: number) {
  const key = `${currency}:${fractionDigits}`;
  let value = formatters.get(key);
  if (!value) {
    value = new Intl.NumberFormat("en-US", {
      style: "currency",
      currency,
      minimumFractionDigits: fractionDigits,
      maximumFractionDigits: 2,
    });
    formatters.set(key, value);
  }
  return value;
}

/** Integer minor units to a full currency string, e.g. 3000 → "$30.00". */
export function formatMoney(minor: number, currency = "USD"): string {
  return formatter(currency, 2).format(minor / 100);
}

/** Like formatMoney but drops ".00" from whole amounts, e.g. 3000 → "$30", 4550 → "$45.50". */
export function formatDollars(minor: number, currency = "USD"): string {
  return formatter(currency, minor % 100 === 0 ? 0 : 2).format(minor / 100);
}

/** A gift is whole dollars a day, billed monthly as the daily amount × 30, up to $30 a day (G-1, G-2). */
export const BILLING_DAYS = 30;
export const MAX_DAILY_DOLLARS = 30;
const MONTHLY_MINOR_PER_DAILY_DOLLAR = 100 * BILLING_DAYS;

/** Whether a monthly amount is a gift a donor can choose today. */
export function isGiftAmount(minor: number): boolean {
  return (
    Number.isInteger(minor) &&
    minor >= MONTHLY_MINOR_PER_DAILY_DOLLAR &&
    minor <= MAX_DAILY_DOLLARS * MONTHLY_MINOR_PER_DAILY_DOLLAR &&
    minor % MONTHLY_MINOR_PER_DAILY_DOLLAR === 0
  );
}

/** The monthly charge for a typed whole-dollar daily amount ("3" or "$3"), or null outside $1–$30 a day. */
export function dailyGiftMinor(input: string): number | null {
  const match = /^\$?(\d{1,2})$/.exec(input.trim());
  if (!match) return null;
  const minor = Number(match[1]) * MONTHLY_MINOR_PER_DAILY_DOLLAR;
  return isGiftAmount(minor) ? minor : null;
}

/**
 * The monthly charge and its daily framing, which must always be shown at equal weight. A gift
 * recorded before the daily rule may not divide evenly, so its daily figure is approximate.
 */
export function monthlyFraming(minor: number): { monthly: string; daily: string } {
  const daily = minor % BILLING_DAYS === 0 && (minor / BILLING_DAYS) % 100 === 0;
  return {
    monthly: `${formatDollars(minor)}/month`,
    daily: `${daily ? "" : "about "}${formatDollars(Math.round(minor / BILLING_DAYS))} a day`,
  };
}

/**
 * Parses a typed dollar amount into exact integer cents. Accepts what people naturally type
 * ("$30", "1,000", "30.", "30.5") but never more than two decimal places.
 */
export function parseDollars(input: string): number | null {
  const match = /^\$?(\d{1,3}(?:,\d{3})+|\d+)(?:\.(\d{0,2}))?$/.exec(input.trim());
  if (!match) return null;
  const minor = Number(match[1].replaceAll(",", "")) * 100 + Number((match[2] ?? "").padEnd(2, "0"));
  return Number.isSafeInteger(minor) ? minor : null;
}
