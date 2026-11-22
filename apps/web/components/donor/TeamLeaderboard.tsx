"use client";
import { useState } from "react";
import type { Schema } from "@/lib/api";
import { formatMoney } from "@/lib/money";

type Team = Schema<"TeamLeaderboardRow">;

/** Minutes as hours to one decimal place, e.g. 90 → "1.5 hours". */
export function hours(minutes: number): string {
  const value = Math.round(minutes / 6) / 10;
  return `${value.toLocaleString("en-US")} ${value === 1 ? "hour" : "hours"}`;
}

const teamViews = {
  giving: { label: "Combined active giving", sort: (t: Team) => t.annualized_minor },
  streak: { label: "Average streak", sort: (t: Team) => t.avg_streak_days },
  volunteer: { label: "Volunteer hours", sort: (t: Team) => t.volunteer_minutes },
} as const;

/** The company team leaderboard; the partner sees it in Our People and employees see it in their account. */
export function TeamLeaderboard({ teams, onRefresh }: { teams: Team[]; onRefresh?: () => void }) {
  const [view, setView] = useState<keyof typeof teamViews>("giving");
  const incomplete = teams.some((team) => !team.metrics_verified);
  const ranked = [...teams].sort((a, b) => {
    if (view !== "volunteer") {
      if (a.metrics_verified !== b.metrics_verified) return a.metrics_verified ? -1 : 1;
      if (!a.metrics_verified) return a.name.localeCompare(b.name);
    }
    return teamViews[view].sort(b) - teamViews[view].sort(a) || a.name.localeCompare(b.name);
  });
  const rank = (t: Team) => ranked.filter((o) => teamViews[view].sort(o) > teamViews[view].sort(t)).length + 1;
  return (
    <>
      <label>
        Rank teams by
        <select value={view} onChange={(event) => setView(event.target.value as keyof typeof teamViews)}>
          {Object.entries(teamViews).map(([key, v]) => (
            <option key={key} value={key}>
              {v.label}
            </option>
          ))}
        </select>
      </label>
      {/* Keyboard focus is required for horizontal scrolling in Safari. */}
      {/* biome-ignore lint/a11y/noNoninteractiveTabindex: this is an interactive scroll region. */}
      <section className="tableScroll" tabIndex={0} aria-label="Team leaderboard table">
        <table className="dataTable">
          <caption className="visuallyHidden">Team leaderboard ranked by {teamViews[view].label.toLowerCase()}</caption>
          <thead>
            <tr>
              <th scope="col">Rank</th>
              <th scope="col">Team</th>
              <th scope="col" className="numeric">
                Giving a year
              </th>
              <th scope="col" className="numeric">
                Average streak
              </th>
              <th scope="col" className="numeric">
                Volunteer hours
              </th>
            </tr>
          </thead>
          <tbody>
            {ranked.map((t) => (
              <tr key={t.team_id}>
                <td>{incomplete && view !== "volunteer" ? "—" : rank(t)}</td>
                <td>{t.name}</td>
                <td className="numeric">
                  {t.metrics_verified ? formatMoney(t.annualized_minor) : "Awaiting verification"}
                </td>
                <td className="numeric">
                  {t.metrics_verified ? `${t.avg_streak_days} days` : "Awaiting verification"}
                </td>
                <td className="numeric">{hours(t.volunteer_minutes)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </section>
      <p className="note">
        Teams with fewer than 5 members are not shown.
        {incomplete && " Giving and streak rankings await verified figures for every team."}
      </p>
      {incomplete && onRefresh && (
        <button type="button" className="secondary" onClick={onRefresh}>
          Refresh team figures
        </button>
      )}
    </>
  );
}
