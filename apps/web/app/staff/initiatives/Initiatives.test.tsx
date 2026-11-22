import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import InitiativeEditor, { initiativeInput } from "./[initiativeId]/InitiativeEditor";
import Initiatives from "./Initiatives";

vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn() }) }));

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const initiative = {
  id: "i-1",
  slug: "reno-meals",
  title: "Reno meals",
  summary: "Hot meals.",
  type: "FOOD",
  location: "Reno, NV",
  region: "Northern Nevada",
  counties: ["Washoe"],
  started_on: "2026-03-01",
  status: "ACTIVE",
  committed_minor: 2_500_000,
  partner_signup_id: "",
  board_approval_source_id: "",
  board_approved_on: "",
  publish_state: "DRAFT",
  published_at: null,
  in_rotation: true,
  updated_at: "2026-10-01T12:00:00Z",
};

function stubApi() {
  const fetch = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (init?.method === "POST") return Promise.resolve(Response.json({ updated: true }));
    if (url.includes("?offset="))
      return Promise.resolve(Response.json({ initiatives: [initiative], next_offset: null }));
    return Promise.resolve(
      Response.json({
        ...initiative,
        metrics: [
          {
            position: 0,
            label: "Meals",
            value: 1200,
            unit: "PEOPLE",
            verification_note: "",
            verified_on: "",
            publish_state: "DRAFT",
          },
        ],
        spend: [{ category: "Food", amount_minor: 90_000 }],
      }),
    );
  });
  vi.stubGlobal("fetch", fetch);
  return fetch;
}

function posted(fetch: ReturnType<typeof stubApi>, suffix: string) {
  const call = fetch.mock.calls.find(([u, i]) => String(u).endsWith(suffix) && i?.method === "POST");
  return call ? JSON.parse(String(call[1]?.body ?? "null")) : undefined;
}

const confirmed = async (name: string) => {
  fireEvent.click(await screen.findByRole("button", { name }));
  fireEvent.click(screen.getByRole("button", { name: /^Confirm / }));
};

it("lists initiatives with their state", async () => {
  stubApi();
  render(<Initiatives />);
  expect(await screen.findByRole("link", { name: "Reno meals" })).toHaveAttribute("href", "/staff/initiatives/i-1");
  expect(screen.getByText(/Draft · Active · Reno, NV/)).toBeInTheDocument();
});

it("reads the editor form into an initiative", () => {
  const form = new FormData();
  for (const [name, value] of Object.entries({
    slug: "reno-meals",
    title: "Reno meals",
    summary: "Hot meals.",
    type: "FOOD",
    location: "Reno, NV",
    region: "Northern Nevada",
    counties: "Washoe, Storey ,",
    started_on: "2026-03-01",
    status: "ACTIVE",
    committed: "$25,000",
    partner: "",
  }))
    form.set(name, value);
  expect(initiativeInput(form)).toMatchObject({ counties: ["Washoe", "Storey"], committed_minor: 2_500_000 });
  form.set("approval_source", "8f9e0d1c-2b3a-4c5d-8e6f-7a8b9c0d1e2f");
  form.set("approved_on", "2026-02-03");
  expect(initiativeInput(form)).toMatchObject({
    board_approval_source_id: "8f9e0d1c-2b3a-4c5d-8e6f-7a8b9c0d1e2f",
    board_approved_on: "2026-02-03",
  });
  form.set("approved_on", "");
  expect(initiativeInput(form)).toBe("Enter both the board-approval source ID and the approval date, or neither.");
  form.set("committed", "lots");
  expect(initiativeInput(form)).toBe("Enter the committed amount in dollars.");
});

it("edits metrics, spend, rotation, and publishing", async () => {
  const fetch = stubApi();
  render(<InitiativeEditor initiative="i-1" />);
  expect(await screen.findByText(/Meals: 1,200 people · Draft/)).toBeInTheDocument();

  fireEvent.change(screen.getByLabelText("Label"), { target: { value: "Spent on food" } });
  fireEvent.change(screen.getByLabelText("Value"), { target: { value: "1,500.50" } });
  fireEvent.change(screen.getByLabelText("Unit"), { target: { value: "USD_CENTS" } });
  fireEvent.change(screen.getByLabelText("Position"), { target: { value: "1" } });
  fireEvent.click(screen.getByRole("button", { name: "Save metric" }));
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Metric 2 saved as a draft."));
  expect(posted(fetch, "/i-1/metrics/1")).toMatchObject({ position: 1, value: 150_050, unit: "USD_CENTS" });

  fireEvent.click(screen.getByRole("button", { name: "Exclude from rotation" }));
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Excluded from the daily rotation."));
  expect(posted(fetch, "/i-1/rotation")).toEqual({ in_rotation: false });

  await confirmed("Remove Food");
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Food removed."));
  expect(posted(fetch, "/i-1/spend/delete")).toEqual({ category: "Food" });

  await confirmed("Publish initiative");
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Initiative published."));
});

it("pages forward by the server's offset and back by one page", async () => {
  const fetch = vi.fn(async (url: string) =>
    Response.json(
      String(url).endsWith("offset=50")
        ? { initiatives: [{ ...initiative, id: "i-2", title: "Later" }], next_offset: null }
        : { initiatives: [initiative], next_offset: 50 },
    ),
  );
  vi.stubGlobal("fetch", fetch);
  render(<Initiatives />);
  fireEvent.click(await screen.findByRole("button", { name: "Older initiatives" }));
  expect(await screen.findByRole("link", { name: "Later" })).toBeVisible();
  fireEvent.click(screen.getByRole("button", { name: "Newer initiatives" }));
  expect(await screen.findByRole("link", { name: "Reno meals" })).toBeVisible();
  expect(fetch).toHaveBeenLastCalledWith("/api/donor/staff/initiatives?offset=0", expect.anything());
});

it("says when there are no initiatives, when access is missing, and retries a failure", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(Response.json({ initiatives: [], next_offset: null })));
  const empty = render(<Initiatives />);
  expect(await screen.findByText("No initiatives yet.")).toBeVisible();
  empty.unmount();
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(Response.json({ detail: "no" }, { status: 403 })));
  const denied = render(<Initiatives />);
  expect(await screen.findByText("Initiatives require Impact Ops access.")).toBeVisible();
  denied.unmount();
  vi.stubGlobal(
    "fetch",
    vi
      .fn()
      .mockResolvedValueOnce(Response.json({ detail: "boom" }, { status: 500 }))
      .mockResolvedValue(Response.json({ initiatives: [initiative], next_offset: null })),
  );
  render(<Initiatives />);
  fireEvent.click(await screen.findByRole("button", { name: "Retry loading" }));
  expect(await screen.findByRole("link", { name: "Reno meals" })).toBeVisible();
});

it("shows the start date as a calendar day regardless of zone", async () => {
  stubApi();
  render(<Initiatives />);
  expect(await screen.findByText(/Started Mar 1, 2026/)).toBeInTheDocument();
});

it.each([
  ["Publish initiative", "/i-1/publish"],
  ["Remove Food", "/i-1/spend/delete"],
  ["Delete metric 1", "/i-1/metrics/0/delete"],
])("does nothing when %s is kept", async (button, suffix) => {
  const fetch = stubApi();
  render(<InitiativeEditor initiative="i-1" />);
  fireEvent.click(await screen.findByRole("button", { name: button }));
  expect(screen.getByRole("button", { name: /^Confirm / })).toHaveFocus();
  fireEvent.click(screen.getByRole("button", { name: /^Keep/ }));
  expect(screen.getByRole("button", { name: button })).toHaveFocus();
  expect(posted(fetch, suffix)).toBeUndefined();
});

it("confirms before publishing a spend category", async () => {
  const fetch = vi.fn((_input: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method === "POST") return Promise.resolve(Response.json({ updated: true }));
    return Promise.resolve(
      Response.json({
        ...initiative,
        metrics: [],
        spend: [{ category: "Food", amount_minor: 90_000, publish_state: "DRAFT" }],
      }),
    );
  });
  vi.stubGlobal("fetch", fetch);
  render(<InitiativeEditor initiative="i-1" />);
  await confirmed("Publish Food");
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Spend published."));
  expect(posted(fetch as ReturnType<typeof stubApi>, "/i-1/spend/publish")).toEqual({ category: "Food" });
});

it("clears the last success message when a new entry is invalid", async () => {
  stubApi();
  render(<InitiativeEditor initiative="i-1" />);
  fireEvent.click(await screen.findByRole("button", { name: "Exclude from rotation" }));
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Excluded from the daily rotation."));
  fireEvent.change(screen.getByLabelText("Label"), { target: { value: "Meals" } });
  fireEvent.change(screen.getByLabelText("Value"), { target: { value: "lots" } });
  fireEvent.click(screen.getByRole("button", { name: "Save metric" }));
  expect(screen.getByRole("alert")).toHaveTextContent("Enter the metric as a whole number.");
  expect(screen.getByRole("status")).toBeEmptyDOMElement();
});

it("rejects a metric too large to store exactly", async () => {
  const fetch = stubApi();
  render(<InitiativeEditor initiative="i-1" />);
  await screen.findByText(/Meals: 1,200 people/);
  fireEvent.change(screen.getByLabelText("Label"), { target: { value: "Meals" } });
  fireEvent.change(screen.getByLabelText("Value"), { target: { value: "9007199254740993" } });
  fireEvent.click(screen.getByRole("button", { name: "Save metric" }));
  expect(screen.getByRole("alert")).toHaveTextContent("Enter the metric as a whole number.");
  expect(posted(fetch, "/i-1/metrics/0")).toBeUndefined();
});

it("keeps unsaved edits across a rotation change but refreshes the form after saving it", async () => {
  let current = { ...initiative, in_rotation: true, updated_at: "2026-10-01T12:00:00Z", metrics: [], spend: [] };
  const fetch = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (init?.method === "POST") {
      current = url.endsWith("/rotation")
        ? { ...current, in_rotation: false, updated_at: "2026-10-02T12:00:00Z" }
        : { ...current, title: "Saved title", updated_at: "2026-10-03T12:00:00Z" };
      return Promise.resolve(Response.json({ updated: true }));
    }
    return Promise.resolve(Response.json(current));
  });
  vi.stubGlobal("fetch", fetch);
  render(<InitiativeEditor initiative="i-1" />);
  const title = await screen.findByLabelText("Title");
  fireEvent.change(title, { target: { value: "Unsaved edit" } });
  fireEvent.click(screen.getByRole("button", { name: "Exclude from rotation" }));
  await screen.findByRole("button", { name: "Include in rotation" });
  expect(screen.getByLabelText("Title")).toHaveValue("Unsaved edit");
  fireEvent.click(screen.getByRole("button", { name: "Save initiative" }));
  await waitFor(() => expect(screen.getByLabelText("Title")).toHaveValue("Saved title"));
});
