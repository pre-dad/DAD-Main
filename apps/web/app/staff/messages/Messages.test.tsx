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
