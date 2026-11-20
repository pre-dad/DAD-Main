import { expect, it, vi } from "vitest";
import About from "./page";

const permanentRedirect = vi.hoisted(() => vi.fn());
vi.mock("next/navigation", () => ({ permanentRedirect }));

it("sends the legacy /about address permanently to /how-it-works", () => {
  About();
  expect(permanentRedirect).toHaveBeenCalledWith("/how-it-works");
});
