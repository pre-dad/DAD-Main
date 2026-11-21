import { renderHook } from "@testing-library/react";
import { StrictMode } from "react";
import { afterEach, beforeEach, expect, it } from "vitest";
import { useHashToken } from "./useHashToken";

beforeEach(() => window.history.replaceState(null, "", "/reset#token=abc&next=%2Faccount"));
afterEach(() => sessionStorage.clear());

it("reads the link and removes it from the address bar", () => {
  const { result } = renderHook(() => useHashToken());
  expect(result.current).toMatchObject({ token: "abc", next: "/account" });
  expect(window.location.hash).toBe("");
});

it("reports an empty value when the link carried none", () => {
  window.history.replaceState(null, "", "/reset");
  const { result } = renderHook(() => useHashToken());
  expect(result.current.token).toBe("");
});

it("keeps the value found on the first run when StrictMode replays the effect", () => {
  const { result } = renderHook(() => useHashToken(), { wrapper: StrictMode });
  expect(result.current.token).toBe("abc");
});

it("survives a reload until cleared when asked to keep it", () => {
  renderHook(() => useHashToken("keep"));
  window.history.replaceState(null, "", "/reset");
  const reloaded = renderHook(() => useHashToken("keep"));
  expect(reloaded.result.current.token).toBe("abc");
  reloaded.result.current.clear();
  const again = renderHook(() => useHashToken("keep"));
  expect(again.result.current.token).toBe("");
});
