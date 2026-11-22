import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import FoundingClaims from "./FoundingClaims";

afterEach(() => vi.unstubAllGlobals());

// jsdom's File has no text(); browsers do.
const csv = (body: string) =>
  Object.assign(new File([body], "waitlist.csv", { type: "text/csv" }), { text: async () => body });

function submit(body: string) {
  fireEvent.change(screen.getByLabelText("Sign-ups CSV"), { target: { files: [csv(body)] } });
  // jsdom's required-file validation cannot see files set this way, so submit directly.
  const form = screen.getByRole("button", { name: "Import sign-ups" }).closest("form");
  if (!form) throw new Error("Missing import form");
  fireEvent.submit(form);
}

it("sends the CSV and reports how many addresses were imported", async () => {
  const fetch = vi.fn().mockResolvedValue(Response.json({ rows: 2 }));
  vi.stubGlobal("fetch", fetch);
  render(<FoundingClaims />);
  const body = "email,signed_up_on,source\na@example.org,2025-03-01,WAITLIST\n";
  submit(body);
  expect(await screen.findByText(/Imported 2 addresses/)).toBeTruthy();
  expect(JSON.parse(fetch.mock.calls[0][1].body)).toEqual({ claims_csv: body });
});

it("lists each problem when the import is refused", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(
      Response.json(
        {
          detail: "The spreadsheet has problems",
          errors: [{ sheet: "claims", row: 2, column: "signed_up_on", reason: "invalid_date" }],
        },
        { status: 422 },
      ),
    ),
  );
  render(<FoundingClaims />);
  submit("email,signed_up_on,source\na@example.org,2027-02-01,WAITLIST\n");
  const problems = await screen.findByRole("list", { name: "Spreadsheet problems" });
  expect(problems.textContent).toContain("Row 2, column signed_up_on");
  expect(problems.textContent).toContain("invalid_date");
});

it("rejects a file over 4 MB without sending it", () => {
  const fetch = vi.fn();
  vi.stubGlobal("fetch", fetch);
  render(<FoundingClaims />);
  const big = new File(["x"], "big.csv", { type: "text/csv" });
  Object.defineProperty(big, "size", { value: 4 * 1024 * 1024 + 1 });
  fireEvent.change(screen.getByLabelText("Sign-ups CSV"), { target: { files: [big] } });
  const form = screen.getByRole("button", { name: "Import sign-ups" }).closest("form") as HTMLFormElement;
  fireEvent.submit(form);
  expect(screen.getByRole("alert")).toHaveTextContent("The CSV must be at most 4 MB.");
  expect(fetch).not.toHaveBeenCalled();
});
