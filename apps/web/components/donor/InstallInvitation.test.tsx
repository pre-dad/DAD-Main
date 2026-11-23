import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { InstallInvitation } from "./InstallInvitation";

afterEach(() => {
  vi.unstubAllGlobals();
  window.history.replaceState({}, "", "/");
});

it.each(["/r/ALEX42", "/r/ALEX42?via=irl", "/c/ACME42"])("copies only the canonical invitation %s", async (path) => {
  const writeText = vi.fn().mockResolvedValue(undefined);
  vi.stubGlobal("navigator", { clipboard: { writeText } });
  window.history.replaceState({}, "", "/?campaign=private#ignored");
  render(<InstallInvitation path={path} />);
  expect(writeText).not.toHaveBeenCalled();
  fireEvent.click(screen.getByText("Keep this invitation for the app"));
  fireEvent.click(screen.getByRole("button", { name: "Copy invitation for the app" }));
  expect(await screen.findByText("Invitation copied.")).toBeVisible();
  expect(writeText).toHaveBeenCalledExactlyOnceWith(new URL(path, window.location.origin).href);
  expect(screen.getByLabelText("Your invitation link")).toHaveValue(new URL(path, window.location.origin).href);
});

it("keeps a selectable invitation when clipboard access fails", async () => {
  vi.stubGlobal("navigator", { clipboard: { writeText: vi.fn().mockRejectedValue(new Error("denied")) } });
  render(<InstallInvitation path="/r/ALEX42?via=irl" />);
  fireEvent.click(screen.getByText("Keep this invitation for the app"));
  fireEvent.click(screen.getByRole("button", { name: "Copy invitation for the app" }));
  expect(await screen.findByText(/Select and copy your invitation link/)).toHaveAttribute("aria-live", "polite");
  expect(screen.getByLabelText("Your invitation link")).toHaveValue(`${window.location.origin}/r/ALEX42?via=irl`);
  expect(screen.getByLabelText("Your invitation link")).toHaveAttribute("readonly");
  expect(screen.queryByText("Invitation copied.")).not.toBeInTheDocument();
});
