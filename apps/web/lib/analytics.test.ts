import { afterEach, expect, it, vi } from "vitest";
import { analyticsAllowed, analyticsChoice, observe, observeAPI } from "./analytics";

afterEach(() => {
  localStorage.clear();
  sessionStorage.clear();
  vi.unstubAllGlobals();
});
it("collects nothing without consent and stops after withdrawal", () => {
  const fetch = vi.fn().mockResolvedValue(new Response("{}"));
  vi.stubGlobal("fetch", fetch);
  observe("app_opened");
  expect(fetch).not.toHaveBeenCalled();
  analyticsChoice(true);
  observe("app_opened");
  expect(fetch).toHaveBeenCalledTimes(1);
  const batch = JSON.parse(fetch.mock.calls[0][1].body);
  expect(batch.events[0]).toMatchObject({ name: "app_opened", properties: {} });
  expect(batch.events[0].journey_id).toBeTruthy();
  analyticsChoice(false);
  observe("app_opened");
  expect(fetch).toHaveBeenCalledTimes(1);
  expect(sessionStorage.getItem("dad.analytics.journey")).toBeNull();
});
it("never treats checkout navigation as financial completion or copies personal request data", () => {
  const fetch = vi.fn().mockResolvedValue(new Response("{}"));
  vi.stubGlobal("fetch", fetch);
  analyticsChoice(true);
  observeAPI(
    "/donor/checkouts",
    "POST",
    { url: "https://checkout.stripe.com" },
    JSON.stringify({ email: "private@example.org" }),
  );
  const payload = String(fetch.mock.calls[0][1].body);
  expect(payload).toContain("gift_started");
  expect(payload).not.toContain("gift_completed");
  expect(payload).not.toContain("private@example.org");
});
it("counts a single event page as event_viewed but not the invite-only list", () => {
  const fetch = vi.fn().mockResolvedValue(new Response("{}"));
  vi.stubGlobal("fetch", fetch);
  analyticsChoice(true);
  observeAPI("/donor/me/events/invite-only", "GET", {}, undefined);
  expect(fetch).not.toHaveBeenCalled();
  observeAPI("/donor/me/events/e1", "GET", {}, undefined);
  expect(fetch).toHaveBeenCalledTimes(1);
  expect(String(fetch.mock.calls[0][1].body)).toContain("event_viewed");
});
it("stays off when the browser says do not track or global privacy control, even with stored consent", () => {
  analyticsChoice(true);
  expect(analyticsAllowed()).toBe(true);
  vi.stubGlobal("navigator", { ...navigator, doNotTrack: "1" });
  expect(analyticsAllowed()).toBe(false);
  vi.stubGlobal("navigator", { ...navigator, doNotTrack: null, globalPrivacyControl: true });
  expect(analyticsAllowed()).toBe(false);
});
it("treats unreadable storage as no consent", () => {
  vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
    throw new Error("denied");
  });
  expect(analyticsAllowed()).toBe(false);
  vi.restoreAllMocks();
});
it("never throws when an API response has no body to inspect", () => {
  const fetch = vi.fn().mockResolvedValue(new Response("{}"));
  vi.stubGlobal("fetch", fetch);
  analyticsChoice(true);
  expect(() => observeAPI("/donor/events/e1/reserve", "POST", null)).not.toThrow();
  expect(String(fetch.mock.calls[0][1].body)).toContain("event_reserved");
});
