import { fireEvent, render, screen } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import PartnerError from "./error";

it("offers a retry and quotes the error reference inside the site frame", () => {
  const retry = vi.fn();
  render(<PartnerError error={Object.assign(new Error("boom"), { digest: "abc123" })} retry={retry} />);
  expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("This page didn’t load");
  expect(screen.getByText("abc123")).toBeVisible();
  expect(screen.queryByText("boom")).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Try again" }));
  expect(retry).toHaveBeenCalledTimes(1);
});
