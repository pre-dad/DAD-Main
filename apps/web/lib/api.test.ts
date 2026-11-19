import { afterEach, expect, it, vi } from "vitest";
import {
  ApiError,
  api,
  classifyFailure,
  failureMessage,
  OFFLINE_MESSAGE,
  RATE_LIMITED_MESSAGE,
  UNCONFIGURED_MESSAGE,
  unconfirmedMessage,
} from "./api";

afterEach(() => vi.unstubAllGlobals());
const response = () => ({ ok: true, json: async () => ({ ok: true }) });

it("marks JSON request bodies so browsers do not send them as text/plain", async () => {
  const fetch = vi.fn().mockResolvedValue(response());
  vi.stubGlobal("fetch", fetch);
  await api("/donor/example", {
    method: "POST",
    body: JSON.stringify({ amount_minor: 3000 }),
    headers: { "Idempotency-Key": "stable" },
  });
  const request = fetch.mock.calls[0][1];
  expect(new Headers(request.headers).get("Content-Type")).toBe("application/json");
  expect(new Headers(request.headers).get("Idempotency-Key")).toBe("stable");
  expect(request.credentials).toBe("include");
});

it("preserves explicit content types and Headers instances", async () => {
  const fetch = vi.fn().mockResolvedValue(response());
  vi.stubGlobal("fetch", fetch);
  await api("/example", {
    method: "POST",
    body: "some,csv",
    headers: new Headers({ "Content-Type": "text/csv", "X-Request": "value" }),
  });
  const headers = new Headers(fetch.mock.calls[0][1].headers);
  expect(headers.get("Content-Type")).toBe("text/csv");
  expect(headers.get("X-Request")).toBe("value");
});

it("lets the browser provide FormData boundaries and Blob content types", async () => {
  const fetch = vi.fn().mockResolvedValue(response());
  vi.stubGlobal("fetch", fetch);
  for (const body of [new FormData(), new Blob(["source document"])]) {
    await api("/example", { method: "POST", body });
  }
  expect(fetch.mock.calls.every(([, request]) => !new Headers(request.headers).has("Content-Type"))).toBe(true);
});

it.each([
  [new ApiError("Sign in required", 401), "signedOut"],
  [new ApiError("Forbidden", 403), "forbidden"],
  [new ApiError("Not found", 404), "notFound"],
  [new ApiError("Slow down", 429), "rateLimited"],
  [new ApiError("Bad gateway", 502), "unavailable"],
  [new ApiError("Payments are off", 503, "PAYMENTS_UNCONFIGURED"), "unconfigured"],
  [new ApiError("Overloaded", 503), "unavailable"],
  [new ApiError("Link expired", 410), "rejected"],
  [new TypeError("Failed to fetch"), "offline"],
  [new SyntaxError("Unexpected token <"), "unavailable"],
])("classifies %s as %s", (cause, failure) => {
  expect(classifyFailure(cause)).toBe(failure);
});

it("never shows a transport error to a donor", () => {
  expect(failureMessage(new TypeError("Failed to fetch"), "Please retry.")).toBe(OFFLINE_MESSAGE);
  expect(failureMessage(new ApiError("Link expired", 401), "Please retry.")).toBe("Link expired");
  expect(failureMessage(new SyntaxError("Unexpected token <"), "Please retry.")).toBe("Please retry.");
  expect(failureMessage(new ApiError("Amount too small", 422), "Please retry.")).toBe("Amount too small");
  expect(failureMessage(new ApiError("Internal", 500), "Please retry.")).toBe("Please retry.");
  expect(failureMessage(new ApiError("Slow down", 429), "Please retry.")).toBe(RATE_LIMITED_MESSAGE);
});

it("keeps an uncertain command's statement while explaining the failure", () => {
  const statement = "Cancellation is not confirmed. Retry the same request.";
  expect(unconfirmedMessage(new TypeError("Failed to fetch"), statement)).toBe(`${statement} ${OFFLINE_MESSAGE}`);
  expect(unconfirmedMessage(new ApiError("Slow down", 429), statement)).toBe(`${statement} ${RATE_LIMITED_MESSAGE}`);
  expect(unconfirmedMessage(new ApiError("Internal", 503), statement)).toBe(statement);
});

it("states definitively that a switched-off capability did nothing, and never invites a retry", () => {
  const off = new ApiError("Payments are not configured", 503, "PAYMENTS_UNCONFIGURED");
  expect(failureMessage(off, "Please retry.")).toBe(UNCONFIGURED_MESSAGE);
  expect(unconfirmedMessage(off, "Refund is not confirmed. Do not issue another refund.")).toBe(UNCONFIGURED_MESSAGE);
  expect(UNCONFIGURED_MESSAGE).not.toMatch(/retry/i);
});

it("reads the coded 503 shape and per-row CSV errors from the API", async () => {
  vi.stubGlobal(
    "fetch",
    vi
      .fn()
      .mockResolvedValueOnce(
        Response.json({ detail: "Payments are not configured", code: "PAYMENTS_UNCONFIGURED" }, { status: 503 }),
      )
      .mockResolvedValueOnce(
        Response.json(
          { detail: "Invalid spreadsheet", errors: [{ row: 3, column: "unit", reason: "unknown unit" }] },
          { status: 422 },
        ),
      ),
  );
  await expect(api("/donor/checkouts", { method: "POST", body: "{}" })).rejects.toMatchObject({
    status: 503,
    code: "PAYMENTS_UNCONFIGURED",
  });
  await expect(api("/donor/staff/founding-claims", { method: "POST", body: "{}" })).rejects.toMatchObject({
    status: 422,
    errors: [{ row: 3, column: "unit", reason: "unknown unit" }],
  });
});

it("keeps the report's daily quota explanation instead of promising a one-minute retry", () => {
  const message = "Today's new report limit has been reached. Try again after midnight UTC.";
  expect(failureMessage(new ApiError(message, 429, "PARTNER_REPORT_DAILY_LIMIT"), "Unavailable")).toBe(message);
});
