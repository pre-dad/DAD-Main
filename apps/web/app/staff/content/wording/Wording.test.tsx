import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import Wording from "./Wording";

afterEach(() => vi.unstubAllGlobals());

const wording = {
  badges: [
    {
      key: "WT",
      title: "Winning Team",
      text: "Your team finishes a month at #1",
      default_title: "Winning Team",
      default_text: "Your team finishes a month at #1",
      custom: false,
    },
  ],
  milestones: [
    {
      key: "30",
      title: "A month of giving",
      text: "Thirty days of showing up.",
      default_title: "Your 30-day giving streak",
      default_text: "You have kept your giving streak going for 30 days.",
      custom: true,
    },
  ],
};

function serve() {
  const fetch = vi
    .fn()
    .mockImplementation(async (url: string, init?: RequestInit) =>
      init?.method === "POST"
        ? Response.json({ saved: true })
        : Response.json(url.endsWith("/rules") ? { badges: [], milestones: [7, 30] } : wording),
    );
  vi.stubGlobal("fetch", fetch);
  return fetch;
}

const saves = (fetch: ReturnType<typeof serve>) =>
  fetch.mock.calls.filter(([, init]) => init?.method === "POST").map(([, init]) => JSON.parse(init.body));

it("saves a badge's trimmed wording", async () => {
  const fetch = serve();
  render(<Wording />);
  fireEvent.change((await screen.findAllByLabelText("Title"))[0], { target: { value: " Top Team " } });
  fireEvent.click(screen.getByRole("button", { name: "Save badge WT" }));
  expect(await screen.findByText("Saved the wording for badge WT.")).toBeVisible();
  expect(saves(fetch)).toEqual([
    { kind: "BADGE", key: "WT", title: "Top Team", text: "Your team finishes a month at #1" },
  ]);
});

it("restores a milestone's default wording", async () => {
  const fetch = serve();
  render(<Wording />);
  expect(await screen.findByText(/Default: Your 30-day giving streak/)).toBeVisible();
  fireEvent.click(screen.getByRole("button", { name: "Restore default for 30-day milestone" }));
  expect(await screen.findByText("Restored the default wording for 30-day milestone.")).toBeVisible();
  expect(saves(fetch)).toEqual([{ kind: "MILESTONE", key: "30", title: "", text: "" }]);
});

it.each([
  ["Title", "   "],
  ["How to earn it", "\t "],
])("rejects whitespace-only %s instead of silently restoring the default", async (label, value) => {
  const fetch = serve();
  render(<Wording />);
  fireEvent.change((await screen.findAllByLabelText(label))[0], { target: { value } });
  fireEvent.click(screen.getByRole("button", { name: "Save badge WT" }));
  expect(await screen.findByText(/Enter both a title and text for badge WT/)).toBeVisible();
  expect(saves(fetch)).toEqual([]);
});

it("keeps the form and says nothing changed when saving fails", async () => {
  const fetch = vi
    .fn()
    .mockImplementation(async (url: string, init?: RequestInit) =>
      init?.method === "POST"
        ? Response.json({ detail: "Unavailable" }, { status: 503 })
        : Response.json(url.endsWith("/rules") ? { badges: [], milestones: [7, 30] } : wording),
    );
  vi.stubGlobal("fetch", fetch);
  render(<Wording />);
  fireEvent.change((await screen.findAllByLabelText("Title"))[0], { target: { value: "Top Team" } });
  fireEvent.click(screen.getByRole("button", { name: "Save badge WT" }));
  expect(await screen.findByText("The wording could not be saved. Nothing was changed.")).toBeVisible();
  expect(screen.getAllByLabelText("Title")[0]).toHaveValue("Top Team");
});
