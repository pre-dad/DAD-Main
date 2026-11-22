import { fireEvent, render, screen, within } from "@testing-library/react";
import { expect, it } from "vitest";
import { TeamLeaderboard } from "./TeamLeaderboard";

it("keeps stale giving and streak figures out of rankings while showing recorded volunteer hours", () => {
  render(
    <TeamLeaderboard
      teams={[
        {
          team_id: "fresh",
          name: "Fresh",
          metrics_verified: true,
          active_givers: 5,
          annualized_minor: 180000,
          avg_streak_days: 10,
          volunteer_minutes: 60,
        },
        {
          team_id: "stale",
          name: "Stale",
          metrics_verified: false,
          active_givers: 500,
          annualized_minor: 99999999,
          avg_streak_days: 999,
          volunteer_minutes: 120,
        },
      ]}
    />,
  );
  const stale = screen.getByRole("row", { name: /Stale/ });
  expect(within(stale).getAllByText("Awaiting verification")).toHaveLength(2);
  expect(within(stale).getByText("—")).toBeInTheDocument();
  expect(screen.queryByText(/999 days/)).not.toBeInTheDocument();
  expect(screen.getByText(/rankings await verified figures/)).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Rank teams by"), { target: { value: "volunteer" } });
  expect(within(stale).getByText("1")).toBeInTheDocument();
  expect(within(stale).getByText("2 hours")).toBeInTheDocument();
});

const team = (id: string, over: Partial<Parameters<typeof TeamLeaderboard>[0]["teams"][number]>) => ({
  team_id: id,
  name: id,
  metrics_verified: true,
  active_givers: 5,
  annualized_minor: 0,
  avg_streak_days: 0,
  volunteer_minutes: 0,
  ...over,
});

it("orders unverified teams by name so hidden figures do not leak through the order", () => {
  render(
    <TeamLeaderboard
      teams={[
        team("Zeta", { metrics_verified: false, annualized_minor: 9_000_000 }),
        team("Alpha", { metrics_verified: false, annualized_minor: 1 }),
        team("Mid", { annualized_minor: 500 }),
      ]}
    />,
  );
  const names = screen
    .getAllByRole("row")
    .slice(1)
    .map((row) => within(row).getAllByRole("cell")[1].textContent);
  expect(names).toEqual(["Mid", "Alpha", "Zeta"]);
});

it("gives teams with equal figures the same rank", () => {
  render(
    <TeamLeaderboard
      teams={[
        team("A", { annualized_minor: 900 }),
        team("B", { annualized_minor: 900 }),
        team("C", { annualized_minor: 100 }),
      ]}
    />,
  );
  const ranks = screen
    .getAllByRole("row")
    .slice(1)
    .map((row) => within(row).getAllByRole("cell")[0].textContent);
  expect(ranks).toEqual(["1", "1", "3"]);
});
