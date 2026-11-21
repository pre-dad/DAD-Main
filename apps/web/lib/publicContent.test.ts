// @vitest-environment node
import { afterEach, expect, it, vi } from "vitest";
import { loadInitiative, loadInitiatives, loadOverallSpend } from "./publicContent";

vi.mock("next/server", () => ({ connection: async () => {} }));
afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

it("bounds a stalled spend request and returns the unavailable state", async () => {
  const controller = new AbortController();
  const timeout = vi.spyOn(AbortSignal, "timeout").mockReturnValue(controller.signal);
  vi.stubGlobal(
    "fetch",
    vi.fn(
      (_url, init: RequestInit) =>
        new Promise((_resolve, reject) => {
          init.signal?.addEventListener("abort", () => reject(init.signal?.reason));
        }),
    ),
  );
  const pending = loadOverallSpend();
  await vi.waitFor(() => expect(timeout).toHaveBeenCalledWith(3000));
  controller.abort(new DOMException("Timed out", "TimeoutError"));
  await expect(pending).resolves.toBeNull();
});

it("retains the published spend's shared cache lifetime", async () => {
  const content = { total_minor: 0, categories: [] };
  const fetch = vi.fn().mockResolvedValue(Response.json(content));
  vi.stubGlobal("fetch", fetch);
  await expect(loadOverallSpend()).resolves.toEqual(content);
  expect(fetch.mock.calls[0][1].next).toEqual({ revalidate: 60 });
});

it("tells a missing initiative from an unavailable API", async () => {
  vi.stubGlobal(
    "fetch",
    vi
      .fn()
      .mockResolvedValueOnce(Response.json({ detail: "Record not found" }, { status: 404 }))
      .mockResolvedValueOnce(Response.json({ detail: "Donor database unavailable" }, { status: 503 })),
  );
  await expect(loadInitiative("no-such-plan")).resolves.toBeUndefined();
  await expect(loadInitiative("water")).resolves.toBeNull();
});

it("returns the published initiatives, and the unavailable state for a bad body", async () => {
  vi.stubGlobal(
    "fetch",
    vi
      .fn()
      .mockResolvedValueOnce(Response.json([{ slug: "water" }]))
      .mockResolvedValueOnce(new Response("<html>gateway</html>", { status: 200 })),
  );
  await expect(loadInitiatives()).resolves.toEqual([{ slug: "water" }]);
  await expect(loadInitiatives()).resolves.toBeNull();
});

it("treats a non-JSON spend or initiative body as unavailable", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(() => Promise.resolve(new Response("oops", { status: 200 }))),
  );
  await expect(loadOverallSpend()).resolves.toBeNull();
  await expect(loadInitiative("water")).resolves.toBeNull();
});
