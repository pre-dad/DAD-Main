import { render, screen } from "@testing-library/react";
import { expect, it } from "vitest";
import { useFocusOnSuccess, useStepFocus } from "./useStepFocus";

function Step({ active }: { active: boolean }) {
  const { heading, trigger } = useStepFocus(active);
  return active ? (
    <h2 ref={heading} tabIndex={-1}>
      Confirm
    </h2>
  ) : (
    <button type="button" ref={trigger}>
      Open
    </button>
  );
}

function Done({ succeeded }: { succeeded: boolean }) {
  const heading = useFocusOnSuccess(succeeded);
  return (
    <h1 ref={heading} tabIndex={-1}>
      Page
    </h1>
  );
}

it("moves focus to the step heading on entry and back to the opening control on return", () => {
  const { rerender } = render(<Step active={false} />);
  expect(document.body).toHaveFocus();
  rerender(<Step active />);
  expect(screen.getByRole("heading", { name: "Confirm" })).toHaveFocus();
  rerender(<Step active={false} />);
  expect(screen.getByRole("button", { name: "Open" })).toHaveFocus();
});

it("focuses the page heading once an action succeeds", () => {
  const { rerender } = render(<Done succeeded={false} />);
  expect(screen.getByRole("heading")).not.toHaveFocus();
  rerender(<Done succeeded />);
  expect(screen.getByRole("heading")).toHaveFocus();
});
