import { fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import Profile from "./Profile";

afterEach(() => vi.unstubAllGlobals());

const profile = {
  email: "alex@example.org",
  public_name: "Alex",
  consent_version: "r2-notifications-v1",
  version: 3,
  referral_emails: false,
  marketing_emails: false,
  push_referral_converted: true,
  push_streak_at_risk: true,
  push_streak_milestone: true,
  push_streak_lapsed: true,
  push_badge_earned: true,
  push_gift_processed: true,
  push_event_nearby: true,
  push_event_reminder: true,
  push_event_promoted: true,
  rankings_opted_out: false,
  network_visible: true,
  volunteer: false,
  volunteer_interests: [] as string[],
  zip: "94103",
  city: "San Francisco",
  phone: null,
};

function stubFetch(routes: Record<string, (init?: RequestInit) => Promise<unknown>> = {}) {
  const fetch = vi.fn((url: string, init?: RequestInit) => {
    const route = routes[`${init?.method ?? "GET"} ${url}`];
    if (route) return route(init);
    if (url === "/api/donor/me/profile") return json(profile);
    if (url === "/api/donor/me/sessions") return json({ sessions: [] });
    if (url === "/api/donor/me/passkeys") return json({ passkeys: [] });
    throw new Error(`unexpected ${init?.method ?? "GET"} ${url}`);
  });
  vi.stubGlobal("fetch", fetch);
  return fetch;
}

const sent = (fetch: ReturnType<typeof stubFetch>, url: string) =>
  JSON.parse(String(fetch.mock.calls.find(([called, init]) => called === url && init?.method === "POST")?.[1]?.body));

const json = (body: unknown, status = 200) => Promise.resolve({ ok: status < 400, status, json: async () => body });

it("saves app notification choices alongside the email ones", async () => {
  const fetch = vi.fn((url: string, init?: RequestInit) => {
    if (url === "/api/donor/me/profile") return json(profile);
    if (url === "/api/donor/me/sessions") return json({ sessions: [] });
    if (url === "/api/donor/me/passkeys") return json({ passkeys: [] });
    if (url === "/api/donor/me/preferences" && init?.method === "POST")
      return json({ ...profile, ...JSON.parse(String(init.body)), version: 4 });
    throw new Error(`unexpected ${url}`);
  });
  vi.stubGlobal("fetch", fetch);
  render(<Profile />);
  const app = within(await screen.findByRole("group", { name: "DAD app notifications" }));
  const milestone = app.getByRole("checkbox", { name: "When my giving streak reaches a milestone" });
  expect(app.getByRole("checkbox", { name: "When someone I invited gives for the first time" })).toBeChecked();
  expect(app.getByRole("checkbox", { name: "When a declined payment puts my giving streak at risk" })).toBeChecked();
  expect(milestone).toBeChecked();
  fireEvent.click(milestone);
  fireEvent.click(screen.getByRole("button", { name: "Save preferences" }));
  expect(await screen.findByText("Preferences saved.")).toHaveAttribute("role", "status");
  const saved = fetch.mock.calls.find(([url]) => url === "/api/donor/me/preferences");
  expect(JSON.parse(String(saved?.[1]?.body))).toEqual({
    version: 3,
    referral_emails: false,
    marketing_emails: false,
    push_referral_converted: true,
    push_streak_at_risk: true,
    push_streak_milestone: false,
    push_streak_lapsed: true,
    push_badge_earned: true,
    push_gift_processed: true,
    push_event_nearby: true,
    push_event_reminder: true,
    push_event_promoted: true,
    rankings_opted_out: false,
    network_visible: true,
    volunteer: false,
    volunteer_interests: [],
  });
  expect(milestone).not.toBeChecked();
});

it("saves the streak-lapsed and badge pushes, rankings and network visibility", async () => {
  const fetch = stubFetch({
    "POST /api/donor/me/preferences": (init) => json({ ...profile, ...JSON.parse(String(init?.body)), version: 4 }),
  });
  render(<Profile />);
  const app = within(await screen.findByRole("group", { name: "DAD app notifications" }));
  fireEvent.click(app.getByRole("checkbox", { name: "When my giving streak ends" }));
  fireEvent.click(app.getByRole("checkbox", { name: "When I earn a badge" }));
  const privacy = within(screen.getByRole("group", { name: "Rankings and network" }));
  const rankings = privacy.getByRole("checkbox", { name: "Rank my giving streak nationally and in my city" });
  expect(rankings).toBeChecked();
  fireEvent.click(rankings);
  fireEvent.click(privacy.getByRole("checkbox", { name: "Show my public nickname to the person who invited me" }));
  fireEvent.click(screen.getByRole("button", { name: "Save preferences" }));
  expect(await screen.findByText("Preferences saved.")).toBeVisible();
  expect(sent(fetch, "/api/donor/me/preferences")).toMatchObject({
    push_streak_lapsed: false,
    push_badge_earned: false,
    rankings_opted_out: true,
    network_visible: false,
  });
  expect(rankings).not.toBeChecked();
});

it("saves the event pushes and offers nearby events only to volunteers", async () => {
  const fetch = stubFetch({
    "POST /api/donor/me/preferences": (init) => json({ ...profile, ...JSON.parse(String(init?.body)), version: 4 }),
  });
  render(<Profile />);
  const app = within(await screen.findByRole("group", { name: "DAD app notifications" }));
  const nearby = "When a volunteer event is announced near me";
  expect(app.queryByRole("checkbox", { name: nearby })).toBeNull();
  fireEvent.click(app.getByRole("checkbox", { name: "Before an event I signed up for" }));
  fireEvent.click(app.getByRole("checkbox", { name: "When a waitlist spot opens for me" }));
  fireEvent.click(screen.getByRole("checkbox", { name: "I volunteer" }));
  fireEvent.click(app.getByRole("checkbox", { name: nearby }));
  fireEvent.click(screen.getByRole("button", { name: "Save preferences" }));
  expect(await screen.findByText("Preferences saved.")).toBeVisible();
  expect(sent(fetch, "/api/donor/me/preferences")).toMatchObject({
    volunteer: true,
    push_event_nearby: false,
    push_event_reminder: false,
    push_event_promoted: false,
  });
});

it("opts in to volunteering and saves free-form interests, keeping ones saved elsewhere", async () => {
  const fetch = stubFetch({
    "GET /api/donor/me/profile": () => json({ ...profile, volunteer_interests: ["from-the-app"] }),
    "POST /api/donor/me/preferences": (init) => json({ ...profile, ...JSON.parse(String(init?.body)), version: 4 }),
  });
  render(<Profile />);
  const volunteering = within(await screen.findByRole("group", { name: "Volunteering" }));
  expect(screen.queryByRole("group", { name: "What interests you?" })).toBeNull();
  fireEvent.click(volunteering.getByRole("checkbox", { name: "I volunteer" }));
  const interests = within(screen.getByRole("group", { name: "What interests you?" }));
  expect(interests.getByRole("button", { name: "Remove from-the-app" })).toBeVisible();
  const box = interests.getByRole("textbox", { name: "Add an interest" });
  fireEvent.change(box, { target: { value: "  Dog walking  " } });
  fireEvent.keyDown(box, { key: "Enter" });
  expect(box).toHaveValue("");
  expect(box).toHaveFocus();
  fireEvent.change(box, { target: { value: "Gardening" } });
  fireEvent.click(interests.getByRole("button", { name: "Add interest" }));
  fireEvent.click(interests.getByRole("button", { name: "Remove Gardening" }));
  fireEvent.click(screen.getByRole("button", { name: "Save preferences" }));
  expect(await screen.findByText("Preferences saved.")).toBeVisible();
  expect(sent(fetch, "/api/donor/me/preferences")).toMatchObject({
    volunteer: true,
    volunteer_interests: ["from-the-app", "Dog walking"],
  });
});

it("saves ZIP, city and phone", async () => {
  const fetch = stubFetch({
    "POST /api/donor/me/profile": (init) => json({ ...profile, ...JSON.parse(String(init?.body)) }),
  });
  render(<Profile />);
  const details = within(await screen.findByRole("group", { name: "Your details" }));
  expect(details.getByRole("textbox", { name: "ZIP code" })).toHaveValue("94103");
  fireEvent.change(details.getByRole("textbox", { name: "ZIP code" }), { target: { value: "10001" } });
  fireEvent.change(details.getByRole("textbox", { name: "City" }), { target: { value: "New York" } });
  fireEvent.change(details.getByRole("textbox", { name: "Phone" }), { target: { value: "+12125550123" } });
  fireEvent.click(details.getByRole("button", { name: "Save details" }));
  expect(await screen.findByText("Your details were saved.")).toBeVisible();
  expect(sent(fetch, "/api/donor/me/profile")).toEqual({ zip: "10001", city: "New York", phone: "+12125550123" });
  expect(screen.getByRole("textbox", { name: "City" })).toHaveValue("New York");
});

it("deletes the account after confirmation", async () => {
  const fetch = stubFetch({ "POST /api/donor/me/delete": () => json({ deleted: true }) });
  const confirm = vi.fn(() => true);
  vi.stubGlobal("confirm", confirm);
  render(<Profile />);
  fireEvent.click(await screen.findByRole("button", { name: "Delete my account" }));
  expect(confirm).toHaveBeenCalledOnce();
  expect(await screen.findByText(/Your account has been deleted/)).toHaveAttribute("role", "status");
  expect(fetch.mock.calls.some(([url]) => url === "/api/donor/me/delete")).toBe(true);
});

it("leaves the account alone when deletion is not confirmed", async () => {
  const fetch = stubFetch();
  vi.stubGlobal("confirm", () => false);
  render(<Profile />);
  fireEvent.click(await screen.findByRole("button", { name: "Delete my account" }));
  expect(fetch.mock.calls.some(([url]) => url === "/api/donor/me/delete")).toBe(false);
});

it("explains that renewing giving must be cancelled before deletion", async () => {
  stubFetch({
    "POST /api/donor/me/delete": () =>
      json(
        { detail: "Cancel your monthly giving, or let a checkout in progress finish, before deleting your account" },
        409,
      ),
  });
  vi.stubGlobal("confirm", () => true);
  render(<Profile />);
  fireEvent.click(await screen.findByRole("button", { name: "Delete my account" }));
  expect(
    await screen.findByText(
      "Cancel your monthly giving, or let a checkout in progress finish, before deleting your account",
    ),
  ).toHaveAttribute("role", "alert");
  expect(screen.getByRole("link", { name: "cancel it" })).toHaveAttribute("href", "/account/recurring");
});

it("signs out of this device only", async () => {
  const fetch = vi.fn((url: string, init?: RequestInit) => {
    if (url === "/api/donor/me/profile") return json(profile);
    if (url === "/api/donor/me/sessions") return json({ sessions: [] });
    if (url === "/api/donor/me/passkeys") return json({ passkeys: [] });
    if (url === "/api/donor/logout" && init?.method === "POST") return json({ signed_out: true });
    throw new Error(`unexpected ${url}`);
  });
  vi.stubGlobal("fetch", fetch);
  render(<Profile />);
  fireEvent.click(await screen.findByRole("button", { name: "Sign out of this device" }));
  expect(await screen.findByRole("link", { name: "Sign in to manage your profile" })).toBeVisible();
  expect(fetch.mock.calls.some(([url]) => url === "/api/donor/logout-all")).toBe(false);
});

it("saves the gift-processed push choice independently of financial email", async () => {
  const fetch = stubFetch({
    "POST /api/donor/me/preferences": (init) => json({ ...profile, ...JSON.parse(String(init?.body)), version: 4 }),
  });
  render(<Profile />);
  const gift = await screen.findByRole("checkbox", { name: "When my gift is processed" });
  expect(gift).toBeChecked();
  fireEvent.click(gift);
  fireEvent.click(screen.getByRole("button", { name: "Save preferences" }));
  expect(await screen.findByText("Preferences saved.")).toBeVisible();
  expect(sent(fetch, "/api/donor/me/preferences")).toMatchObject({ push_gift_processed: false });
});
