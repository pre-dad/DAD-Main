import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import Employer from "./Employer";

afterEach(() => vi.unstubAllGlobals());

it("enrolls an unlinked donor with a company code", async () => {
  const fetch = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (url.endsWith("/donor/me/team") && init?.method !== "POST") {
      return Promise.resolve(Response.json({ partner_id: "", organization: "", leaderboard: [] }));
    }
    return Promise.resolve(Response.json({ organization: "Delta Inc" }));
  });
  vi.stubGlobal("fetch", fetch);
  render(<Employer />);
  await waitFor(() => expect(screen.getByLabelText("Company code")).toBeInTheDocument());
  fireEvent.change(screen.getByLabelText("Company code"), { target: { value: "DELTA" } });
  fireEvent.click(screen.getByRole("button", { name: "Link my employer" }));
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("linked to Delta Inc"));
  expect(fetch.mock.calls.some(([u, i]) => String(u).endsWith("/donor/me/employer") && i?.method === "POST")).toBe(
    true,
  );
});

const team = (name: string, annualized: number, streak: number, minutes: number) => ({
  team_id: name,
  name,
  active_givers: 5,
  metrics_verified: true,
  annualized_minor: annualized,
  avg_streak_days: streak,
  volunteer_minutes: minutes,
});

it("shows the linked employer, team, and company leaderboard in each ranking", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(() =>
      Promise.resolve(
        Response.json({
          partner_id: "p1",
          organization: "Delta Inc",
          team_name: "Engineering",
          leaderboard: [team("Engineering", 180000, 10, 600), team("Sales", 108000, 90, 60)],
        }),
      ),
    ),
  );
  render(<Employer />);
  await waitFor(() => expect(screen.getByRole("button", { name: "Change team" })).toBeInTheDocument());
  const firstTeam = () => screen.getAllByRole("row")[1];
  expect(firstTeam()).toHaveTextContent("1Engineering$1,800.00");
  fireEvent.change(screen.getByLabelText("Rank teams by"), { target: { value: "streak" } });
  expect(firstTeam()).toHaveTextContent("1Sales");
  fireEvent.change(screen.getByLabelText("Rank teams by"), { target: { value: "volunteer" } });
  expect(firstTeam()).toHaveTextContent("1Engineering");
  expect(screen.getByText("10 hours")).toBeInTheDocument();
});

it("asks a signed-out visitor to sign in and links back to the dashboard", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => Response.json({ detail: "Sign in required" }, { status: 401 })),
  );
  render(<Employer />);
  expect(await screen.findByRole("link", { name: "Sign in to link your employer" })).toHaveAttribute(
    "href",
    "/account/sign-in",
  );
  expect(screen.getByRole("link", { name: "My dashboard" })).toHaveAttribute("href", "/account/dashboard");
  expect(screen.queryByLabelText("Company code")).not.toBeInTheDocument();
});

it("reports the API's reason when a company code cannot be linked", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) =>
      init?.method === "POST"
        ? Response.json({ detail: "Your account is already linked to another employer" }, { status: 409 })
        : Response.json({ partner_id: "", organization: "", leaderboard: [] }),
    ),
  );
  render(<Employer />);
  fireEvent.change(await screen.findByLabelText("Company code"), { target: { value: "DELTA" } });
  fireEvent.click(screen.getByRole("button", { name: "Link my employer" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("Your account is already linked to another employer");
});

it("offers a retry when the employer details cannot load", async () => {
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(Response.json({}, { status: 500 }))
    .mockResolvedValueOnce(Response.json({ partner_id: "", organization: "", leaderboard: [] }));
  vi.stubGlobal("fetch", fetch);
  render(<Employer />);
  expect(await screen.findByRole("alert")).toHaveTextContent("could not be loaded");
  fireEvent.click(screen.getByRole("button", { name: "Retry loading" }));
  expect(await screen.findByLabelText("Company code")).toBeInTheDocument();
});
