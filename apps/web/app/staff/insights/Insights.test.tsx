import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import StaffInsights from "./Insights";

afterEach(() => vi.unstubAllGlobals());

const ME = "11111111-1111-4111-8111-111111111111";
const OTHER = "22222222-2222-4222-8222-222222222222";
const ID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
const spec = [
  { type: "Heading", title: "Your impact" },
  { type: "Metric", bind: "/impact/given_minor", format: "usd_minor", caption: "given" },
];

const row = (state: string, author = OTHER, extra = {}) => ({
  id: ID,
  state,
  catalog_version: 1,
  author_id: author,
  author_name: author === ME ? "Me" : "Alex Author",
  approver_id: null,
  approver_name: null,
  reject_reason: null,
  created_at: "2026-11-01T12:00:00Z",
  updated_at: "2026-11-01T12:00:00Z",
  submitted_at: null,
  decided_at: null,
  retired_at: null,
  ...extra,
});

type Call = { method: string; path: string; body?: unknown };

/** A staff API that serves one layout and records every call. */
function serve(initial: ReturnType<typeof row>, replies: Record<string, () => Response> = {}) {
  const calls: Call[] = [];
  let current = initial;
  const fetch = vi.fn(async (input: string, init: RequestInit = {}) => {
    const path = String(input).replace("/api", "");
    const method = init.method ?? "GET";
    calls.push({ method, path, body: typeof init.body === "string" ? JSON.parse(init.body) : undefined });
    if (replies[`${method} ${path}`]) return replies[`${method} ${path}`]();
    if (path === "/donor/staff/me") return Response.json({ id: ME, name: "Me", permissions: ["content.publish"] });
    if (path.startsWith("/donor/staff/insights?") || (path === "/donor/staff/insights" && method === "GET"))
      return Response.json({ specs: [current], next_cursor: null });
    if (path === `/donor/staff/insights/${ID}` && method === "GET") return Response.json({ ...current, spec });
    if (method === "POST" && path.startsWith(`/donor/staff/insights/${ID}/`)) {
      const action = path.split("/").pop();
      const state =
        { submit: "pending", approve: "live", reject: "rejected", retire: "retired" }[action ?? ""] ?? current.state;
      current = { ...current, state };
      return Response.json({ ...current, spec });
    }
    return Response.json({ detail: "unexpected" }, { status: 500 });
  });
  vi.stubGlobal("fetch", fetch);
  return calls;
}

async function open(label: RegExp) {
  fireEvent.click(await screen.findByRole("button", { name: label }));
  return screen.findByLabelText("Layout JSON", { selector: "textarea[rows='14']" });
}

it("lets a pending layout's author see why they cannot approve it, with no approve or reject controls", async () => {
  const calls = serve(row("pending", ME));
  render(<StaffInsights />);
  await open(/Waiting for approval/);
  expect(await screen.findByText(/a different staff member must approve or reject/)).toBeVisible();
  expect(screen.queryByRole("button", { name: /Approve/ })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: /Reject/ })).not.toBeInTheDocument();
  expect(calls.some((call) => call.method === "POST")).toBe(false);
});

it("asks before approving another member's layout, then approves it", async () => {
  const calls = serve(row("pending"));
  render(<StaffInsights />);
  await open(/Waiting for approval/);
  fireEvent.click(await screen.findByRole("button", { name: /^Approve/ }));
  expect(calls.some((call) => call.method === "POST")).toBe(false);
  fireEvent.click(screen.getByRole("button", { name: /Approve and make live/ }));
  await waitFor(() => expect(calls.some((call) => call.method === "POST")).toBe(true));
  expect(calls.find((call) => call.method === "POST")).toMatchObject({ path: `/donor/staff/insights/${ID}/approve` });
  expect(await screen.findByText("Approve done.")).toBeVisible();
});

it("needs a reason, and confirmation, to reject", async () => {
  const calls = serve(row("pending"));
  render(<StaffInsights />);
  await open(/Waiting for approval/);
  const reject = await screen.findByRole("button", { name: /^Reject/ });
  expect(reject).toBeDisabled();
  fireEvent.change(screen.getByLabelText("Reason for rejecting"), { target: { value: "  Wording needs review " } });
  fireEvent.click(screen.getByRole("button", { name: /^Reject/ }));
  fireEvent.click(screen.getByRole("button", { name: /Reject layout/ }));
  await waitFor(() => expect(calls.some((call) => call.path.endsWith("/reject"))).toBe(true));
  expect(calls.find((call) => call.path.endsWith("/reject"))?.body).toEqual({ reason: "Wording needs review" });
});

it("retires the live layout only after confirmation", async () => {
  const calls = serve(row("live"));
  render(<StaffInsights />);
  await open(/Live/);
  fireEvent.click(await screen.findByRole("button", { name: /^Retire/ }));
  expect(calls.some((call) => call.method === "POST")).toBe(false);
  fireEvent.click(screen.getByRole("button", { name: /Retire layout/ }));
  await waitFor(() => expect(calls.some((call) => call.path.endsWith("/retire"))).toBe(true));
});

it("lets the author edit and submit a draft, and shows the preview with sample figures", async () => {
  const calls = serve(row("draft", ME));
  render(<StaffInsights />);
  const editor = await open(/Draft/);
  expect(editor).not.toHaveAttribute("readonly");
  const detail = await screen.findByRole("region", { name: /layout by/ });
  const preview = within(detail).getByRole("region", { name: "Preview with sample figures" });
  expect(within(preview).getByText("$45.00")).toBeVisible();
  fireEvent.change(editor, { target: { value: JSON.stringify([{ type: "Heading", title: "Edited" }]) } });
  expect(within(preview).getByRole("heading", { level: 3, name: "Edited" })).toBeVisible();
  fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
  await waitFor(() =>
    expect(calls.some((call) => call.method === "POST" && call.path === `/donor/staff/insights/${ID}`)).toBe(true),
  );
  expect(calls.find((call) => call.method === "POST")?.body).toEqual({ spec: [{ type: "Heading", title: "Edited" }] });
  fireEvent.click(screen.getByRole("button", { name: "Submit for approval" }));
  await waitFor(() => expect(calls.some((call) => call.path.endsWith("/submit"))).toBe(true));
});

it("keeps another member's draft read-only with no actions", async () => {
  serve(row("draft"));
  render(<StaffInsights />);
  const editor = await open(/Draft/);
  expect(editor).toHaveAttribute("readonly");
  expect(screen.queryByRole("button", { name: "Submit for approval" })).not.toBeInTheDocument();
});

it("shows each catalog problem with its path and code when a new layout is refused", async () => {
  const calls = serve(row("draft"), {
    "POST /donor/staff/insights": () =>
      Response.json(
        {
          detail: "The layout does not pass the catalog rules",
          errors: [{ path: "/0/navigate", code: "NAV_FORBIDDEN", message: "navigate must be an approved in-app path" }],
        },
        { status: 422 },
      ),
  });
  render(<StaffInsights />);
  await screen.findByRole("button", { name: /Draft/ });
  fireEvent.change(screen.getByLabelText("Layout JSON"), {
    target: { value: '[{"type":"LinkButton","navigate":"/partner"}]' },
  });
  fireEvent.click(screen.getByRole("button", { name: "Save as draft" }));
  const list = await screen.findByRole("list", { name: "Catalog problems" });
  expect(within(list).getByText("/0/navigate")).toBeVisible();
  expect(within(list).getByText("NAV_FORBIDDEN")).toBeVisible();
  expect(calls.filter((call) => call.method === "POST")).toHaveLength(1);
});

it("refuses text that is not a JSON array without calling the API", async () => {
  const calls = serve(row("draft"));
  render(<StaffInsights />);
  await screen.findByRole("button", { name: /Draft/ });
  fireEvent.change(screen.getByLabelText("Layout JSON"), { target: { value: "{nope" } });
  fireEvent.click(screen.getByRole("button", { name: "Save as draft" }));
  await waitFor(() =>
    expect(screen.getAllByRole("alert").some((alert) => alert.textContent === "This is not valid JSON.")).toBe(true),
  );
  expect(calls.some((call) => call.method === "POST")).toBe(false);
});

it("points a signed-out visitor to staff sign-in", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => Response.json({}, { status: 401 })),
  );
  render(<StaffInsights />);
  expect(await screen.findByRole("link", { name: "Staff sign-in" })).toHaveAttribute("href", "/staff/sign-in");
});
