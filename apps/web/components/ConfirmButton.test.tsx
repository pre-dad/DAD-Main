import { fireEvent, render, screen } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { ConfirmButton } from "./ConfirmButton";

const setup = (onConfirm = vi.fn()) => {
  render(
    <ConfirmButton confirmLabel="Confirm removal" target="of Ada" prompt="Remove Ada?" onConfirm={onConfirm}>
      Remove
    </ConfirmButton>,
  );
  return onConfirm;
};

it("asks first, moves focus to Confirm, and only acts on Confirm", () => {
  const onConfirm = setup();
  fireEvent.click(screen.getByRole("button", { name: "Remove of Ada" }));
  expect(onConfirm).not.toHaveBeenCalled();
  const confirm = screen.getByRole("button", { name: "Confirm removal of Ada" });
  expect(confirm).toHaveFocus();
  expect(confirm).toHaveAccessibleDescription("Remove Ada?");
  fireEvent.click(confirm);
  expect(onConfirm).toHaveBeenCalledOnce();
  expect(screen.getByRole("button", { name: "Remove of Ada" })).toBeVisible();
});

it("returns focus to the trigger on Keep without acting", () => {
  const onConfirm = setup();
  fireEvent.click(screen.getByRole("button", { name: "Remove of Ada" }));
  fireEvent.click(screen.getByRole("button", { name: "Keep of Ada" }));
  expect(onConfirm).not.toHaveBeenCalled();
  expect(screen.getByRole("button", { name: "Remove of Ada" })).toHaveFocus();
});
