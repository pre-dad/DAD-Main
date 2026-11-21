import { act, render, screen } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { RefreshButton } from "./RefreshButton";

const refresh = vi.fn();
vi.mock("next/navigation", () => ({ useRouter: () => ({ refresh }) }));

it("announces that it is checking again while the refresh is pending", async () => {
  let finish: () => void = () => {};
  refresh.mockImplementation(() => new Promise<void>((resolve) => (finish = resolve)));
  render(<RefreshButton>Try again</RefreshButton>);
  expect(screen.getByRole("status")).toBeEmptyDOMElement();
  await act(async () => screen.getByRole("button", { name: "Try again" }).click());
  expect(screen.getByRole("status")).toHaveTextContent("Checking again…");
  expect(screen.getByRole("button")).toHaveAttribute("aria-disabled", "true");
  await act(async () => finish());
});
