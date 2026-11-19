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
