import { fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import Badges from "./Badges";

afterEach(() => vi.unstubAllGlobals());

it("separates earned badges from how to earn the rest", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(() =>
      Promise.resolve(
        Response.json({
          badges: [
            {
              code: "FOUNDING",
              title: "Founding Donor",
              rule: "Give before launch",
              perk: "Founders events",
              earned: true,
              earned_at: "2026-10-01T12:00:00Z",
              city: null,
              display_title: "Founding Donor",
            },
            {
              code: "CITY50",
              title: "Top 50 City",
              rule: "Rank in the top 50 in your city",
              perk: "",
              earned: true,
              earned_at: "2026-10-02T12:00:00Z",
              city: "Chicago",
              display_title: "Top 50 in Chicago",
            },
            {
              code: "STREAK_30",
              title: "30-day streak",
              rule: "Give for 30 days in a row",
              perk: "",
              earned: false,
              earned_at: null,
              city: null,
              display_title: "30-day streak",
            },
          ],
        }),
      ),
    ),
  );
  render(<Badges />);
  const earned = (await screen.findByRole("heading", { name: "Earned" })).closest("section");
  const unearned = screen.getByRole("heading", { name: "Still to earn" }).closest("section");
  if (!earned || !unearned) throw new Error("Missing sections");
  expect(within(earned).getByText("Founding Donor")).toBeVisible();
  expect(within(earned).getByText("Founders events")).toBeVisible();
  expect(within(earned).getByText("Top 50 in Chicago")).toBeVisible();
  expect(within(earned).queryByText("Top 50 City")).not.toBeInTheDocument();
  expect(within(unearned).getByText("Give for 30 days in a row")).toBeVisible();
  expect(within(earned).queryByText("30-day streak")).not.toBeInTheDocument();
});

const badge = (extra: Record<string, unknown>) => ({
  code: "B",
  title: "Badge",
  rule: "A rule",
  perk: "",
  earned: false,
  earned_at: null,
  city: null,
  display_title: "Badge",
  ...extra,
});

it("invites a donor with no earned badges and still lists what to earn", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(Response.json({ badges: [badge({})] })));
  render(<Badges />);
  expect(await screen.findByText(/No badges yet/)).toBeVisible();
  expect(screen.getByText("A rule")).toBeVisible();
});

it("asks a signed-out donor to sign in", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(Response.json({}, { status: 401 })));
  render(<Badges />);
  expect(await screen.findByRole("link", { name: "Sign in to see your badges" })).toBeVisible();
});

it("retries after a load failure", async () => {
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(Response.json({}, { status: 500 }))
    .mockResolvedValue(Response.json({ badges: [badge({ earned: true, earned_at: "2026-10-01T12:00:00Z" })] }));
  vi.stubGlobal("fetch", fetch);
  render(<Badges />);
  expect(await screen.findByText(/could not be loaded/)).toHaveAttribute("role", "alert");
  fireEvent.click(screen.getByRole("button", { name: "Retry loading" }));
  expect(await screen.findByText("Earned Oct 1, 2026")).toBeVisible();
  expect(screen.queryByRole("button", { name: "Retry loading" })).not.toBeInTheDocument();
});

it("shows badge artwork as decoration", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(
      Response.json({
        badges: [
          badge({ code: "E", earned: true, earned_at: "2026-10-01T12:00:00Z", artwork: "/badges/e.png" }),
          badge({ code: "U", artwork: "/badges/u.png" }),
        ],
      }),
    ),
  );
  const { container } = render(<Badges />);
  await screen.findByRole("heading", { name: "Earned" });
  const images = [...container.querySelectorAll("img")];
  expect(images.map((image) => image.getAttribute("alt"))).toEqual(["", ""]);
  expect(images.map((image) => image.getAttribute("src"))).toEqual(["/badges/e.png", "/badges/u.png"]);
});
