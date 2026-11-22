import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { Rules } from "./Rules";

afterEach(() => vi.unstubAllGlobals());

const rules = {
  badges: [
    { code: "CITY50", signal: "city_rank", threshold: 50, min_population: 100, artwork: "" },
    { code: "7", signal: "streak_days", threshold: 7, min_population: 1, artwork: "" },
  ],
  milestones: [7, 30],
};

it("saves a rank badge's minimum population and leaves other badges at one", async () => {
  const fetch = vi
    .fn()
    .mockImplementation(async (_url: string, init?: RequestInit) =>
      init?.method === "POST" ? Response.json({ saved: true }) : Response.json(rules),
    );
  vi.stubGlobal("fetch", fetch);
  render(<Rules />);
  const population = await screen.findAllByLabelText("Fewest ranked donors before this badge is awarded");
  expect(population).toHaveLength(1);
  fireEvent.change(population[0], { target: { value: "250" } });
  fireEvent.click(screen.getByRole("button", { name: "Save earning rules and artwork" }));
  expect(await screen.findByText("Rules and artwork saved.")).toBeVisible();
  const [, init] = fetch.mock.calls.find(([, i]) => i?.method === "POST") ?? [];
  expect(JSON.parse(init.body).badges).toEqual([
    { code: "CITY50", signal: "city_rank", threshold: 50, min_population: 250, artwork: "" },
    { code: "7", signal: "streak_days", threshold: 7, min_population: 1, artwork: "" },
  ]);
});

function serve() {
  const fetch = vi
    .fn()
    .mockImplementation(async (_url: string, init?: RequestInit) =>
      init?.method === "POST" ? Response.json({ saved: true }) : Response.json(rules),
    );
  vi.stubGlobal("fetch", fetch);
  return fetch;
}

const posts = (fetch: ReturnType<typeof serve>) => fetch.mock.calls.filter(([, init]) => init?.method === "POST");

it.each(["7, 7", "30, 7", "0, 7", "7.5", "seven", "7,,30", ""])(
  "rejects milestone days %j before anything is sent",
  async (value) => {
    const fetch = serve();
    render(<Rules />);
    fireEvent.change(await screen.findByLabelText(/Streak days/), { target: { value } });
    fireEvent.submit(screen.getByRole("button", { name: "Save earning rules and artwork" }).closest("form") as Element);
    expect(await screen.findByText(/Milestone days must be whole numbers from 1/)).toBeVisible();
    expect(posts(fetch)).toHaveLength(0);
  },
);

it("sends increasing whole-number milestones and the edited threshold", async () => {
  const fetch = serve();
  render(<Rules />);
  fireEvent.change(await screen.findByLabelText(/Streak days/), { target: { value: "7, 30, 90" } });
  fireEvent.change(screen.getAllByLabelText("Threshold")[1], { target: { value: "14" } });
  fireEvent.click(screen.getByRole("button", { name: "Save earning rules and artwork" }));
  expect(await screen.findByText("Rules and artwork saved.")).toBeVisible();
  const body = JSON.parse(posts(fetch)[0][1].body);
  expect(body.milestones).toEqual([7, 30, 90]);
  expect(body.badges[1].threshold).toBe(14);
});

it("clears a badge's artwork when asked", async () => {
  const fetch = vi.fn().mockImplementation(async (_url: string, init?: RequestInit) =>
    init?.method === "POST"
      ? Response.json({ saved: true })
      : Response.json({
          ...rules,
          badges: [{ ...rules.badges[1], artwork: "data:image/png;base64,AAAA" }],
        }),
  );
  vi.stubGlobal("fetch", fetch);
  render(<Rules />);
  fireEvent.click(await screen.findByLabelText("Remove artwork"));
  fireEvent.click(screen.getByRole("button", { name: "Save earning rules and artwork" }));
  await screen.findByText("Rules and artwork saved.");
  expect(JSON.parse(posts(fetch)[0][1].body).badges[0].artwork).toBe("");
});

it("keeps typed values while a save is in flight and shows the reloaded rules afterwards", async () => {
  let reads = 0;
  const fetch = vi.fn().mockImplementation(async (_url: string, init?: RequestInit) => {
    if (init?.method === "POST") return Response.json({ saved: true });
    reads += 1;
    return Response.json(reads === 1 ? rules : { ...rules, milestones: [7, 30, 60] });
  });
  vi.stubGlobal("fetch", fetch);
  render(<Rules />);
  fireEvent.click(await screen.findByRole("button", { name: "Save earning rules and artwork" }));
  await waitFor(() => expect(screen.getByLabelText(/Streak days/)).toHaveValue("7, 30, 60"));
});
