import type { Schema } from "@/lib/api";

/** Invented figures for previewing a layout in the staff console. Never read from a real donor. */
export const sampleDashboard: Schema<"DonorDashboard"> = {
  mode: "test",
  state: "ready",
  computed_at: "2027-01-01T00:00:00Z",
  founding_donor: true,
  referrals: { link: 2, irl: 3, total_minor: 12345, link_minor: 10000, irl_minor: 2345, currency: "USD" },
  streak: {
    current: 24,
    at_risk: false,
    grace_ends_at: null,
    ladder: { days: 24, last_milestone: 7, next_milestone: 30, days_to_next: 6, bar: Array(30).fill(true) },
  },
  rank: { national_percentile: 2, city: "Sampletown", city_rank: 7, computed_at: "2027-01-01T00:00:00Z" },
  rankings_opted_out: false,
  network: { active_count: 3, annualized_minor: 108000, currency: "USD" },
  impact: { given_minor: 4500, currency: "USD", initiatives_funded: 4, events_volunteered: 1 },
  insights: null,
} as Schema<"DonorDashboard">;
