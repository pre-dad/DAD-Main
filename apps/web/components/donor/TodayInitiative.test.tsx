import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { TodayInitiative } from "./TodayInitiative";

afterEach(() => vi.unstubAllGlobals());

it("says the initiative is display attribution, not a designated gift (A-5)", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(() =>
      Promise.resolve(
        Response.json({
          initiative_id: "i-1",
          title: "Reno meals",
          type: "Food",
          location: "Reno, NV",
          started_on: "2026-09-01",
          distinct_shown: 1,
          metrics: [],
        }),
      ),
    ),
  );
  render(<TodayInitiative />);
  expect(await screen.findByRole("heading", { name: "Reno meals" })).toBeInTheDocument();
  expect(screen.getByText("Why this initiative?")).toBeInTheDocument();
  expect(screen.getByText(/none is designated to this initiative/)).toBeInTheDocument();
});

const today = {
  initiative_id: "i-1",
  title: "Reno meals",
  type: "Food",
  location: "Reno, NV",
  started_on: "2026-09-01",
  distinct_shown: 1,
  metrics: [{ label: "Served", unit: "PEOPLE", value: 1200, verified_on: null }],
};

function respond(spend: () => Promise<Response>) {
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string) => (url.includes("/spend") ? spend() : Promise.resolve(Response.json(today)))),
  );
}

async function openSpend() {
  render(<TodayInitiative />);
  await screen.findByRole("heading", { name: "Reno meals" });
  const summary = screen.getByText("See every dollar this initiative has spent");
  const details = summary.closest("details") as HTMLDetailsElement;
  details.open = true;
  fireEvent(details, new Event("toggle"));
}

it("formats headline numbers in en-US whatever the browser locale", async () => {
  respond(() => Promise.resolve(Response.json({ total_minor: 0, categories: [] })));
  render(<TodayInitiative />);
  expect(await screen.findByText("1,200 people")).toBeInTheDocument();
});

it("shows a loading status while spending is fetched, then the categories", async () => {
  let finish: (response: Response) => void = () => {};
  respond(() => new Promise<Response>((resolve) => (finish = resolve)));
  await openSpend();
  expect(await screen.findByRole("status")).toHaveTextContent("Loading spending…");
  finish(Response.json({ total_minor: 5000, categories: [{ category: "Food", amount_minor: 5000 }] }));
  expect(await screen.findByText("Food: $50.00")).toBeInTheDocument();
  expect(screen.getByText("Total: $50.00")).toBeInTheDocument();
  expect(screen.queryByText("Loading spending…")).toBeNull();
});

it("says so when no spending has been recorded", async () => {
  respond(() => Promise.resolve(Response.json({ total_minor: 0, categories: [] })));
  await openSpend();
  expect(await screen.findByText("No spending has been recorded yet.")).toBeInTheDocument();
});

it("reports a spending failure", async () => {
  respond(() => Promise.resolve(Response.json({ detail: "down" }, { status: 503 })));
  await openSpend();
  expect(await screen.findByText(/Spending could not be loaded/)).toHaveAttribute("role", "alert");
  expect(screen.queryByText("Loading spending…")).toBeNull();
});
