import { render, screen } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { KeysetPager } from "./KeysetPager";

const labels = { newer: "Newer gifts", older: "Older gifts" };

it("is a labelled navigation landmark holding the page buttons", () => {
  render(<KeysetPager older={vi.fn()} labels={labels} />);
  const nav = screen.getByRole("navigation", { name: "Pagination" });
  expect(nav).toContainElement(screen.getByRole("button", { name: "Older gifts" }));
  expect(screen.queryByRole("button", { name: "Newer gifts" })).toBeNull();
});

it("renders nothing when there is nowhere to page", () => {
  render(<KeysetPager labels={labels} />);
  expect(screen.queryByRole("navigation")).toBeNull();
});
