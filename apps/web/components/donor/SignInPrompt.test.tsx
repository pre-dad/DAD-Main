import { render, screen } from "@testing-library/react";
import { expect, it } from "vitest";
import { SignInPrompt } from "./SignInPrompt";

it("names every way to sign in and links to the sign-in page", () => {
  render(<SignInPrompt>Sign in</SignInPrompt>);
  expect(screen.getByText(/emailed single-use link, a passkey, a phone code or a password/)).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "Sign in" })).toHaveAttribute("href", "/account/sign-in");
});
