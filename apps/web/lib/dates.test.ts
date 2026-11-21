import { expect, it } from "vitest";
import { formatDate, formatDateTime, formatDay, formatLongDay } from "./dates";

it("keeps a calendar date on its own day in every time zone", () => {
  expect(formatDay("2026-09-30")).toBe("Sep 30, 2026");
  expect(formatDay("2027-01-01")).toBe("Jan 1, 2027");
});

it("states an instant to the minute with its zone, and no seconds", () => {
  const text = formatDateTime("2026-10-15T12:00:30Z");
  expect(text).toMatch(/^Oct 1[45], 2026, \d{1,2}:\d{2} [AP]M \S+$/);
  expect(text).not.toMatch(/:\d{2}:\d{2}/);
});

it("states an instant in a given zone whatever the device's", () => {
  expect(formatDateTime("2026-11-14T04:02:00Z", "America/Chicago")).toBe("Nov 13, 2026, 10:02 PM CST");
  expect(formatDateTime("2026-11-14T04:02:00Z", "America/Los_Angeles")).toBe("Nov 13, 2026, 8:02 PM PST");
});

it("falls back to the device's zone for a zone it does not know", () => {
  expect(formatDateTime("2026-11-14T04:02:00Z", "Mars/Olympus_Mons")).toBe(formatDateTime("2026-11-14T04:02:00Z"));
  expect(formatDateTime("2026-11-14T04:02:00Z", "")).toBe(formatDateTime("2026-11-14T04:02:00Z"));
});
it("spells the month out for a calendar date, whatever the device's zone", () => {
  expect(formatLongDay("2026-09-30")).toBe("September 30, 2026");
  expect(formatLongDay("2027-01-01")).toBe("January 1, 2027");
});
it("shows an instant on the reader's calendar day", () => {
  expect(formatDate("2026-10-15T12:00:00Z")).toMatch(/^Oct 1[45], 2026$/);
});
it("returns nothing for a malformed calendar date instead of throwing", () => {
  expect(formatDay("not-a-date")).toBe("");
  expect(formatLongDay("")).toBe("");
});
