import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { OFFLINE_MESSAGE, RATE_LIMITED_MESSAGE } from "./api";
import { resourceMessages, useApiResource } from "./useApiResource";

afterEach(() => vi.unstubAllGlobals());
const reply = (status: number, body: unknown = {}) => ({ ok: status < 400, status, json: async () => body });

it("loads data, reports it once to onLoad, and reloads on request", async () => {
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(reply(200, { n: 1 }))
    .mockResolvedValueOnce(reply(200, { n: 2 }));
  vi.stubGlobal("fetch", fetch);
  const onLoad = vi.fn();
  const { result } = renderHook(() => useApiResource<{ n: number }>("/donor/me/thing", { onLoad }));
  expect(result.current.resource).toEqual({ state: "loading" });
  await waitFor(() => expect(result.current.data).toEqual({ n: 1 }));
  expect(onLoad).toHaveBeenCalledWith({ n: 1 });
  expect(fetch.mock.calls[0][0]).toBe("/api/donor/me/thing");
  expect(fetch.mock.calls[0][1].cache).toBeUndefined();
  act(() => result.current.reload());
  await waitFor(() => expect(result.current.data).toEqual({ n: 2 }));
  expect(onLoad).toHaveBeenCalledTimes(2);
});

it("keeps the loaded data while a reload is in flight, and loads afresh for a new path", async () => {
  let release: (value: unknown) => void = () => undefined;
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(reply(200, { n: 1 }))
    .mockReturnValueOnce(new Promise((resolve) => (release = resolve)))
    .mockResolvedValueOnce(reply(200, { n: 3 }));
  vi.stubGlobal("fetch", fetch);
  const { result, rerender } = renderHook(({ path }) => useApiResource<{ n: number }>(path), {
    initialProps: { path: "/donor/me/thing" },
  });
  await waitFor(() => expect(result.current.data).toEqual({ n: 1 }));
  act(() => result.current.reload());
  expect(fetch).toHaveBeenCalledTimes(2);
  expect(result.current.resource).toEqual({ state: "ready", data: { n: 1 } });
  await act(async () => release(reply(200, { n: 2 })));
  await waitFor(() => expect(result.current.data).toEqual({ n: 2 }));
  rerender({ path: "/donor/me/other" });
  expect(result.current.resource).toEqual({ state: "loading" });
  await waitFor(() => expect(result.current.data).toEqual({ n: 3 }));
});

it("shows a failed reload instead of the data it could not refresh", async () => {
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(reply(200, { n: 1 }))
    .mockResolvedValueOnce(reply(503));
  vi.stubGlobal("fetch", fetch);
  const { result } = renderHook(() => useApiResource<{ n: number }>("/donor/me/thing"));
  await waitFor(() => expect(result.current.data).toEqual({ n: 1 }));
  act(() => result.current.reload());
  await waitFor(() => expect(result.current.resource).toEqual({ state: "error", rateLimited: false }));
  expect(result.current.data).toBeNull();
});

it.each([
  [reply(401), { state: "signedOut" }],
  [reply(403), { state: "forbidden" }],
  [reply(404), { state: "notFound" }],
  [reply(429), { state: "error", rateLimited: true }],
  [reply(503), { state: "error", rateLimited: false }],
  [reply(503, { detail: "Email is off", code: "EMAIL_UNCONFIGURED" }), { state: "unconfigured" }],
])("maps an HTTP failure to a screen state", async (response, expected) => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(response));
  const { result } = renderHook(() => useApiResource("/donor/me/thing"));
  await waitFor(() => expect(result.current.resource).toEqual(expected));
});

it("treats a network failure as offline and never exposes its message", async () => {
  vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("Failed to fetch")));
  const { result } = renderHook(() => useApiResource("/donor/me/thing"));
  await waitFor(() => expect(result.current.resource).toEqual({ state: "offline" }));
  const messages = resourceMessages(result.current.resource, { loading: "Loading…", failure: "Could not load." });
  expect(messages.alert).toBe(OFFLINE_MESSAGE);
  expect(messages.alert).not.toContain("Failed to fetch");
});

it("does not request anything until a path is known", () => {
  const fetch = vi.fn();
  vi.stubGlobal("fetch", fetch);
  renderHook(() => useApiResource(null));
  expect(fetch).not.toHaveBeenCalled();
});

it("lets a page update loaded data after a confirmed action", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(reply(200, { n: 1 })));
  const { result } = renderHook(() => useApiResource<{ n: number }>("/donor/me/thing"));
  await waitFor(() => expect(result.current.data).toEqual({ n: 1 }));
  act(() => result.current.setData((current) => ({ n: current.n + 1 })));
  expect(result.current.data).toEqual({ n: 2 });
});

it("gives each non-ready state its own copy", () => {
  const copy = { loading: "Loading…", failure: "Could not load.", forbidden: "Not permitted." };
  expect(resourceMessages({ state: "loading" }, copy)).toEqual({ status: "Loading…", alert: "" });
  expect(resourceMessages({ state: "forbidden" }, copy).alert).toBe("Not permitted.");
  expect(resourceMessages({ state: "error", rateLimited: true }, copy).alert).toBe(RATE_LIMITED_MESSAGE);
  expect(resourceMessages({ state: "error", rateLimited: false }, copy).alert).toBe("Could not load.");
  expect(resourceMessages({ state: "signedOut" }, copy)).toEqual({ status: "", alert: "" });
  expect(resourceMessages({ state: "unconfigured" }, copy).alert).toMatch(/isn’t available in this environment/);
  expect(resourceMessages({ state: "notFound" }, copy).alert).toBe("Could not load.");
  expect(resourceMessages({ state: "notFound" }, { ...copy, notFound: "No such record." }).alert).toBe(
    "No such record.",
  );
});

it("keeps the previous data on screen while a new path loads when asked to", async () => {
  let release: (value: unknown) => void = () => undefined;
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(reply(200, { n: 1 }))
    .mockReturnValueOnce(new Promise((resolve) => (release = resolve)));
  vi.stubGlobal("fetch", fetch);
  const { result, rerender } = renderHook(({ path }) => useApiResource<{ n: number }>(path, { keepPrevious: true }), {
    initialProps: { path: "/donor/me/thing?page=1" },
  });
  await waitFor(() => expect(result.current.data).toEqual({ n: 1 }));
  rerender({ path: "/donor/me/thing?page=2" });
  expect(result.current.resource).toEqual({ state: "ready", data: { n: 1 } });
  await act(async () => release(reply(200, { n: 2 })));
  await waitFor(() => expect(result.current.data).toEqual({ n: 2 }));
});

it("reports a throwing onLoad as an error, not as being offline", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(reply(200, { n: 1 })));
  const { result } = renderHook(() =>
    useApiResource<{ n: number }>("/x", {
      onLoad: () => {
        throw new Error("caller bug");
      },
    }),
  );
  await waitFor(() => expect(result.current.resource).toEqual({ state: "error", rateLimited: false }));
});
