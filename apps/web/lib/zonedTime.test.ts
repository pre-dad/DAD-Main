import { expect, it } from "vitest";
import { validZone, zonedInstant, zonedLocal } from "./zonedTime";

it("reads a wall clock in the named zone, whatever the browser zone is", () => {
  expect(zonedInstant("2026-11-07T19:00", "America/Chicago")).toBe("2026-11-08T01:00:00Z");
  expect(zonedInstant("2026-11-07T19:00", "America/Los_Angeles")).toBe("2026-11-08T03:00:00Z");
  expect(zonedInstant("2026-07-04T19:00", "America/Chicago")).toBe("2026-07-05T00:00:00Z");
  expect(zonedInstant("2026-11-07T19:00:30", "UTC")).toBe("2026-11-07T19:00:30Z");
});

it("shows an instant as the wall clock in the named zone", () => {
  expect(zonedLocal("2026-11-08T01:00:00Z", "America/Chicago")).toBe("2026-11-07T19:00");
  expect(zonedLocal("2026-11-08T01:00:00Z", "Pacific/Honolulu")).toBe("2026-11-07T15:00");
});

it("round-trips across a daylight-saving change", () => {
  for (const instant of [
    "2026-03-08T07:30:00Z",
    "2026-03-08T09:30:00Z",
    "2026-11-01T05:30:00Z",
    "2026-11-01T08:30:00Z",
  ])
    expect(zonedInstant(zonedLocal(instant, "America/Chicago"), "America/Chicago")).toBe(instant);
});

it("rejects unknown zones and malformed times", () => {
  expect(validZone("Mars/Olympus")).toBe(false);
  expect(validZone("America/Chicago")).toBe(true);
  expect(zonedInstant("2026-11-07T19:00", "Mars/Olympus")).toBeNull();
  expect(zonedInstant("tomorrow", "UTC")).toBeNull();
});
