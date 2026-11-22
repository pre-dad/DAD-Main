import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import Partners, { matchTerms } from "./Partners";

afterEach(() => vi.unstubAllGlobals());

it("lists approved partners with their terms and links to each", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(() =>
      Promise.resolve(
        Response.json({
          partners: [
            {
              id: "p-1",
              organization: "Delta Inc",
              type: "Corporate",
              company_code: "DELTAX7K2QP",
              workforce_size: 1200,
              match_rate_bp: 10000,
              match_cap_minor: 5_000_000,
              approved_at: "2026-09-01T12:00:00Z",
              user_count: 2,
              initiative_count: 1,
            },
          ],
          next_offset: null,
        }),
      ),
    ),
  );
  render(<Partners />);
  const link = await screen.findByRole("link", { name: "Delta Inc" });
  expect(link).toHaveAttribute("href", "/staff/partners/p-1");
  expect(screen.getByText(/100% match up to \$50,000 a year · 1 initiative · 2 portal users/)).toBeInTheDocument();
  await waitFor(() => expect(screen.queryByRole("button", { name: "More partners" })).not.toBeInTheDocument());
});

it("describes match terms", () => {
  expect(matchTerms(0, 0)).toBe("No match");
  expect(matchTerms(5000, 0)).toBe("50% match, uncapped");
  expect(matchTerms(12550, 100_000)).toBe("125.5% match up to $1,000 a year");
});

const partner = (id: string) => ({
  id,
  organization: `Org ${id}`,
  type: "Corporate",
  company_code: `CODE${id}`,
  workforce_size: 10,
  match_rate_bp: 0,
  match_cap_minor: 0,
  approved_at: "2026-09-01T12:00:00Z",
  user_count: 1,
  initiative_count: 0,
});

it("pages forward by the server's offset and back by one page", async () => {
  const fetch = vi.fn(async (url: string) =>
    Response.json(
      url.endsWith("offset=50")
        ? { partners: [partner("late")], next_offset: null }
        : { partners: [partner("early")], next_offset: 50 },
    ),
  );
  vi.stubGlobal("fetch", fetch);
  render(<Partners />);
  fireEvent.click(await screen.findByRole("button", { name: "More partners" }));
  expect(await screen.findByRole("link", { name: "Org late" })).toBeVisible();
  expect(fetch).toHaveBeenLastCalledWith("/api/donor/staff/partners?offset=50", expect.anything());
  fireEvent.click(screen.getByRole("button", { name: "Previous partners" }));
  expect(await screen.findByRole("link", { name: "Org early" })).toBeVisible();
  expect(fetch).toHaveBeenLastCalledWith("/api/donor/staff/partners?offset=0", expect.anything());
});

it("says when no partner is approved and offers a retry after a failure", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(Response.json({ partners: [], next_offset: null })));
  const { unmount } = render(<Partners />);
  expect(await screen.findByText("No partners have been approved yet.")).toBeVisible();
  unmount();
  vi.stubGlobal(
    "fetch",
    vi
      .fn()
      .mockResolvedValueOnce(Response.json({ detail: "boom" }, { status: 500 }))
      .mockResolvedValue(Response.json({ partners: [partner("a")], next_offset: null })),
  );
  render(<Partners />);
  fireEvent.click(await screen.findByRole("button", { name: "Retry loading" }));
  expect(await screen.findByRole("link", { name: "Org a" })).toBeVisible();
});
