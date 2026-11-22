import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { PasswordAccess } from "./PasswordAccess";

afterEach(() => vi.unstubAllGlobals());

it("tells the donor a wrong current password is wrong, not that they are signed out", async () => {
  const fetch = vi.fn(() =>
    Promise.resolve(
      Response.json(
        { detail: "The email or password is incorrect", code: "CURRENT_PASSWORD_INCORRECT" },
        { status: 422 },
      ),
    ),
  );
  vi.stubGlobal("fetch", fetch);
  render(<PasswordAccess setup />);
  fireEvent.click(screen.getByText("Set or change your password"));
  fireEvent.change(screen.getByLabelText(/Current password/), { target: { value: "typo" } });
  fireEvent.change(screen.getByLabelText("New password"), { target: { value: "a long and unique password" } });
  fireEvent.click(screen.getByRole("button", { name: "Save password" }));
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("Your current password is incorrect"));
  expect(fetch).toHaveBeenCalledWith("/api/donor/me/password", expect.objectContaining({ method: "POST" }));
});

it("signs in with email and password and opens the dashboard", async () => {
  const fetch = vi.fn(() => Promise.resolve(Response.json({})));
  const assign = vi.fn();
  vi.stubGlobal("fetch", fetch);
  vi.stubGlobal("location", { ...window.location, assign });
  render(<PasswordAccess />);
  fireEvent.click(screen.getByText("Sign in with email and password"));
  fireEvent.change(screen.getByLabelText("Email"), { target: { value: "a@example.org" } });
  fireEvent.change(screen.getByLabelText("Password"), { target: { value: "a long and unique password" } });
  fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
  await waitFor(() => expect(assign).toHaveBeenCalledWith("/account/dashboard"));
  expect(fetch).toHaveBeenCalledWith(
    "/api/donor/password/sign-in",
    expect.objectContaining({
      body: JSON.stringify({ email: "a@example.org", password: "a long and unique password" }),
    }),
  );
});

it("confirms a saved password and clears the typed passwords from the form", async () => {
  const fetch = vi.fn(() => Promise.resolve(Response.json({})));
  vi.stubGlobal("fetch", fetch);
  render(<PasswordAccess setup />);
  fireEvent.click(screen.getByText("Set or change your password"));
  fireEvent.change(screen.getByLabelText(/Current password/), { target: { value: "old password" } });
  fireEvent.change(screen.getByLabelText("New password"), { target: { value: "a long and unique password" } });
  fireEvent.click(screen.getByRole("button", { name: "Save password" }));
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Password saved."));
  expect(screen.getByLabelText(/Current password/)).toHaveValue("");
  expect(screen.getByLabelText("New password")).toHaveValue("");
  expect(fetch).toHaveBeenCalledWith(
    "/api/donor/me/password",
    expect.objectContaining({
      body: JSON.stringify({ current_password: "old password", password: "a long and unique password" }),
    }),
  );
});
