import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import DeliveryHistory from "./Messages";

afterEach(() => vi.unstubAllGlobals());
const reply = (status: number, body: unknown) => ({ ok: status < 400, status, json: async () => body });

it.each([
  [false, true],
  [true, false],
])("offers a claim email only to an unclaimed donor (claimed: %s)", async (claimed, offered) => {
  let detailsRead = () => {};
  const details = new Promise<void>((resolve) => {
    detailsRead = resolve;
  });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) =>
      url.includes("/messages")
        ? reply(200, { messages: [], next_cursor: null })
        : {
            ok: true,
            status: 200,
            json: async () => {
              detailsRead();
              return { id: "donor-1", claimed };
            },
          },
    ),
  );
  render(<DeliveryHistory donor="donor-1" />);
  expect(await screen.findByText("No transactional messages recorded.")).toBeVisible();
  await details;
  await act(async () => {});
  expect(screen.queryByText("Request account claim email") !== null).toBe(offered);
});

it("pages the delivery history by cursor", async () => {
  const message = (id: string) => ({ id, kind: "RECEIPT", attempts: 1, completed_at: "2026-10-20T12:00:00Z" });
  const fetch = vi.fn(async (url: string) => {
    if (!url.includes("/messages")) return reply(200, { id: "donor-1", claimed: true });
    if (url.includes("cursor=c2")) return reply(200, { messages: [message("m-old")], next_cursor: null });
    return reply(200, { messages: [message("m-new")], next_cursor: "c2" });
  });
  vi.stubGlobal("fetch", fetch);
  render(<DeliveryHistory donor="donor 1" />);
  const older = await screen.findByRole("button", { name: "Older messages" });
  expect(screen.queryByRole("button", { name: "Newer messages" })).not.toBeInTheDocument();
  fireEvent.click(older);
  expect(await screen.findByRole("button", { name: "Newer messages" })).toBeVisible();
  expect(fetch).toHaveBeenCalledWith("/api/donor/staff/donors/donor%201/messages?cursor=c2", expect.anything());
  expect(screen.queryByRole("button", { name: "Older messages" })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Newer messages" }));
  expect(await screen.findByRole("button", { name: "Older messages" })).toBeVisible();
});

const resendable = { id: "m-1", kind: "RECEIPT", attempts: 1, completed_at: "2026-10-20T12:00:00Z" };

function resendBackend(outcomes: (() => unknown)[]) {
  const keys: (string | undefined)[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      if (url.endsWith("/resend")) {
        keys.push(new Headers(init?.headers).get("Idempotency-Key") ?? undefined);
        return (outcomes.shift() ?? (() => reply(200, {})))();
      }
      if (!url.includes("/messages")) return reply(200, { id: "donor-1", claimed: true });
      return reply(200, { messages: [resendable], next_cursor: null });
    }),
  );
  return keys;
}

const resend = async () => {
  const button = await screen.findByRole("button", { name: "Confirm resend" });
  await act(async () => {
    fireEvent.click(button);
  });
};

it("retries an unconfirmed resend with the same key, then uses a new key for the next resend", async () => {
  const keys = resendBackend([() => reply(503, { detail: "down" }), () => reply(200, {}), () => reply(200, {})]);
  render(<DeliveryHistory donor="donor-1" />);
  await resend();
  expect(await screen.findByText(/Resend is not confirmed/)).toHaveAttribute("role", "alert");
  await resend();
  expect(await screen.findByText("Delivery request recorded. This does not confirm email delivery.")).toBeVisible();
  await resend();
  expect(keys).toHaveLength(3);
  expect(keys[1]).toBe(keys[0]);
  expect(keys[2]).not.toBe(keys[0]);
});

it("requests a claim email for an unclaimed donor", async () => {
  const fetch = vi.fn(async (url: string) => {
    if (url.endsWith("/claim-email")) return reply(200, {});
    if (url.includes("/messages")) return reply(200, { messages: [], next_cursor: null });
    return reply(200, { id: "donor-1", claimed: false });
  });
  vi.stubGlobal("fetch", fetch);
  render(<DeliveryHistory donor="donor-1" />);
  fireEvent.click(await screen.findByText("Request account claim email"));
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Confirm claim email request" }));
  });
  expect(await screen.findByText(/Delivery request recorded/)).toBeVisible();
  expect(fetch).toHaveBeenCalledWith(
    "/api/donor/staff/donors/donor-1/claim-email",
    expect.objectContaining({ method: "POST" }),
  );
});
