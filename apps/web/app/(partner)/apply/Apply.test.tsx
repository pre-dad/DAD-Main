import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import Apply from "./Apply";

afterEach(() => vi.unstubAllGlobals());

function fill() {
  fireEvent.change(screen.getByLabelText("Organization name"), { target: { value: "Delta Inc" } });
  fireEvent.change(screen.getByLabelText("Website"), { target: { value: "delta.example" } });
  fireEvent.change(screen.getByLabelText("Approximate workforce size"), { target: { value: "500" } });
  fireEvent.change(screen.getByLabelText("Your name"), { target: { value: "Dana" } });
  fireEvent.change(screen.getByLabelText("Your title"), { target: { value: "Head of Giving" } });
  fireEvent.change(screen.getByLabelText("Work email"), { target: { value: "dana@delta.example" } });
  fireEvent.change(screen.getByLabelText("Phone"), { target: { value: "+14155550123" } });
  fireEvent.click(screen.getByRole("button", { name: "Submit application" }));
}

it("submits a normalized application and shows the pending-review state", async () => {
  const fetch = vi.fn().mockResolvedValue(Response.json({ received: true }));
  vi.stubGlobal("fetch", fetch);
  render(<Apply />);
  fill();
  await waitFor(() => expect(screen.getByRole("heading", { name: /pending review/i })).toBeInTheDocument());
  const submitted = JSON.parse(fetch.mock.calls[0][1].body);
  const body = submitted.application;
  expect(submitted.receipt).toMatch(/^[A-Za-z0-9_-]{43}$/);
  expect(fetch.mock.calls[0][0]).toBe("/api/donor/partner/applications");
  expect(body.website).toBe("https://delta.example");
  expect(body.workforce_size).toBe(500);
  expect(fetch.mock.calls[0][1].headers["Idempotency-Key"]).toBeTruthy();
});

it("surfaces a 422 and re-enables the form for correction", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(Response.json({ detail: "A pending application already exists." }, { status: 422 })),
  );
  render(<Apply />);
  fill();
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("pending application already exists"));
  expect(screen.getByLabelText("Organization name")).toBeEnabled();
});

it("unfreezes and starts a fresh attempt after any rejection, such as a 400", async () => {
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(Response.json({ detail: "Website is not valid." }, { status: 400 }))
    .mockResolvedValueOnce(Response.json({ received: true }));
  vi.stubGlobal("fetch", fetch);
  render(<Apply />);
  fill();
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("Website is not valid."));
  expect(screen.getByLabelText("Organization name")).toBeEnabled();
  fireEvent.click(screen.getByRole("button", { name: "Submit application" }));
  await screen.findByRole("heading", { name: /pending review/i });
  const [first, second] = fetch.mock.calls.map(([, init]) => init);
  expect(second.headers["Idempotency-Key"]).not.toBe(first.headers["Idempotency-Key"]);
  expect(JSON.parse(second.body).receipt).not.toBe(JSON.parse(first.body).receipt);
});

it("uses a generic prompt when a rejection carries no explanation", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("{}", { status: 400 })));
  render(<Apply />);
  fill();
  await waitFor(() => expect(screen.getByRole("alert")).not.toHaveTextContent("highlighted"));
});

it("keeps the same key, receipt and body when an unconfirmed submit is retried", async () => {
  const fetch = vi
    .fn()
    .mockRejectedValueOnce(new TypeError("Failed to fetch"))
    .mockResolvedValueOnce(Response.json({ received: true }));
  vi.stubGlobal("fetch", fetch);
  render(<Apply />);
  fill();
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("not confirmed"));
  fireEvent.click(screen.getByRole("button", { name: "Retry application" }));
  await screen.findByRole("heading", { name: /pending review/i });
  const [first, second] = fetch.mock.calls.map(([, init]) => init);
  expect(second.headers["Idempotency-Key"]).toBe(first.headers["Idempotency-Key"]);
  expect(second.body).toBe(first.body);
});

it("resets the key and receipt after a 409", async () => {
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(Response.json({ detail: "Conflict." }, { status: 409 }))
    .mockResolvedValueOnce(Response.json({ received: true }));
  vi.stubGlobal("fetch", fetch);
  render(<Apply />);
  fill();
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("Conflict."));
  fireEvent.click(screen.getByRole("button", { name: "Submit application" }));
  await screen.findByRole("heading", { name: /pending review/i });
  const [first, second] = fetch.mock.calls.map(([, init]) => init);
  expect(second.headers["Idempotency-Key"]).not.toBe(first.headers["Idempotency-Key"]);
  expect(JSON.parse(second.body).receipt).not.toBe(JSON.parse(first.body).receipt);
});
