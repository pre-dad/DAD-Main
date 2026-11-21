import { render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it } from "vitest";
import { SocialSignInError } from "./SocialSignInError";

afterEach(() => window.history.replaceState(null, "", "/"));

it.each([
  ["1", /didn’t complete/],
  ["unavailable", /isn’t available right now/],
])("explains social_error=%s", async (value, copy) => {
  window.history.replaceState(null, "", `/account/sign-in?social_error=${value}`);
  render(<SocialSignInError />);
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent(copy));
});

it("says nothing without a social sign-in error", () => {
  render(<SocialSignInError />);
  expect(screen.queryByRole("alert")).not.toBeInTheDocument();
});

it("ignores an unknown error code", () => {
  window.history.replaceState(null, "", "/account/sign-in?social_error=<script>");
  render(<SocialSignInError />);
  expect(screen.queryByRole("alert")).not.toBeInTheDocument();
});
