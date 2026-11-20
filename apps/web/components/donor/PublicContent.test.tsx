import { render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import InitiativePage from "@/app/(donor)/initiatives/[slug]/page";
import InitiativesPage from "@/app/(donor)/initiatives/page";
import SpendPage from "@/app/(donor)/spend/page";

const refresh = vi.fn();
vi.mock("next/navigation", async (original) => ({
  ...(await original<typeof import("next/navigation")>()),
  useRouter: () => ({ refresh }),
  notFound: () => {
    throw new Error("NEXT_NOT_FOUND");
  },
}));

// Rendering outside a request: there is no request scope for connection() to wait on.
vi.mock("next/server", () => ({ connection: async () => undefined }));

const initiative = {
  id: "8f9e0d1c-2b3a-4c5d-8e6f-7a8b9c0d1e2f",
  slug: "water",
  title: "Clean water",
  summary: "Wells with a partner.",
  type: "Water",
  location: "Northern region",
  status: "ACTIVE",
  started_on: "2099-05-01",
  partner: "Partner Org",
};
const spend = {
  total_minor: 190_00,
  categories: [
    { category: "Direct aid", amount_minor: 150_00 },
    { category: "Logistics", amount_minor: 40_00 },
  ],
};
afterEach(() => {
  vi.unstubAllGlobals();
  vi.unstubAllEnvs();
  refresh.mockReset();
});
const respond = (body: unknown, status = 200) =>
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response(JSON.stringify(body), { status })),
  );

it("shows spend as category totals with an overall total and no line items", async () => {
  respond(spend);
  render(await SpendPage());
  expect(screen.getByRole("heading", { level: 1, name: "Spend by category" })).toBeVisible();
  expect(screen.getByText("Direct aid")).toBeInTheDocument();
  expect(screen.getByText("$150.00")).toBeInTheDocument();
  expect(screen.getByText("$40.00")).toBeInTheDocument();
  expect(screen.getByText("$190.00")).toBeInTheDocument();
  expect(screen.queryByText(/Illustrative/)).not.toBeInTheDocument();
});

it("fetches the spend totals endpoint with a one-minute shared cache", async () => {
  vi.stubEnv("DONOR_API_BASE_URL", "http://api.internal:8002");
  respond(spend);
  render(await SpendPage());
  const fetch = vi.mocked(globalThis.fetch);
  expect(fetch.mock.calls[0][0]).toBe("http://api.internal:8002/donor/public/spend");
  expect(fetch.mock.calls[0][1]).toMatchObject({ next: { revalidate: 60 } });
});

it.each([
  ["an unreachable API", () => vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("fetch failed")))],
  ["a server error", () => respond({ detail: "boom" }, 503)],
])("offers a retry for %s and never substitutes demonstration numbers", async (_label, fail) => {
  fail();
  render(await SpendPage());
  expect(screen.getByRole("alert")).toHaveTextContent("Published records are unavailable");
  expect(screen.queryByText("$150.00")).not.toBeInTheDocument();
});

it("shows an empty-state message when no spend has been published", async () => {
  respond({ total_minor: 0, categories: [] });
  render(await SpendPage());
  expect(screen.getByText(/No spend has been published/)).toBeInTheDocument();
  expect(screen.queryByRole("alert")).not.toBeInTheDocument();
});

it("links each initiative to its detail page", async () => {
  respond([initiative]);
  render(await InitiativesPage());
  expect(screen.getByRole("link", { name: "Clean water" })).toHaveAttribute("href", "/initiatives/water");
  expect(screen.getByText(/Partner: Partner Org/)).toHaveTextContent("Planned start");
});

it("renders a published initiative and treats an unknown slug as not found", async () => {
  respond(initiative);
  render(await InitiativePage({ params: Promise.resolve({ slug: "water" }), searchParams: Promise.resolve({}) }));
  expect(screen.getByRole("heading", { level: 1, name: "Clean water" })).toBeVisible();
  expect(screen.getByText(/not designated to this initiative/)).toBeVisible();
  respond({ detail: "Record not found" }, 404);
  await expect(
    InitiativePage({ params: Promise.resolve({ slug: "missing" }), searchParams: Promise.resolve({}) }),
  ).rejects.toThrow("NEXT_NOT_FOUND");
});

it("shows a running initiative without its partner when none is linked", async () => {
  respond([{ ...initiative, started_on: "2026-03-01", partner: null }]);
  render(await InitiativesPage());
  expect(screen.queryByText(/Partner:/)).toBeNull();
  expect(screen.getByText(/Started/)).toBeInTheDocument();
  expect(screen.queryByText(/not a recipient enrollment form/)).toBeNull();
});
