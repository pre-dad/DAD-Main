import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { launchDateLabel } from "@/lib/launch";
import SharePage from "./Share";

const sharing = {
  public_name: "Sam",
  link: "https://dad.example/r/code",
  irl_link: "https://dad.example/r/code?via=irl",
};
afterEach(() => {
  vi.unstubAllGlobals();
});
function response(data: unknown) {
  return { ok: true, json: async () => data };
}

it("requires a chosen public nickname and keeps QR off the share page", async () => {
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(response({ public_name: null, link: null, irl_link: null }))
    .mockResolvedValueOnce(response(sharing));
  vi.stubGlobal("fetch", fetch);
  render(<SharePage />);
  fireEvent.change(await screen.findByLabelText("Public first name or nickname"), { target: { value: "Sam" } });
  expect(screen.getByText(/anyone who opens your referral link/)).toBeVisible();
  expect(screen.queryByRole("img")).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Save public name" }));
  expect(await screen.findByLabelText("Your referral link")).toHaveValue(sharing.link);
  expect(JSON.parse(fetch.mock.calls[1][1].body)).toEqual({ public_name: "Sam", replace_code: false });
});

it("opens native sharing with the Link URL and accurate billing disclosure", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(response(sharing)));
  const share = vi.fn().mockResolvedValue(undefined);
  vi.stubGlobal("navigator", { share, onLine: true });
  render(<SharePage />);
  fireEvent.click(await screen.findByRole("button", { name: "Share invitation" }));
  expect(share).toHaveBeenCalledWith(
    expect.objectContaining({ url: sharing.link, text: expect.stringContaining("$30/month") }),
  );
});

it("copies the bare link, announces it, and previews the invitation", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(response(sharing)));
  const writeText = vi.fn().mockResolvedValue(undefined);
  vi.stubGlobal("navigator", { clipboard: { writeText }, onLine: true });
  render(<SharePage />);
  expect(await screen.findByRole("heading", { name: "Sam invited you to DAD" })).toBeVisible();
  expect(screen.getByText(new RegExp(`${launchDateLabel()}\\.`), { selector: "p" })).toBeVisible();
  fireEvent.click(screen.getByRole("button", { name: "Copy link" }));
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Link copied."));
  expect(writeText).toHaveBeenCalledWith(sharing.link);
});

it("requires a focused confirmation step before replacing the link, and restores focus on return", async () => {
  const fetch = vi.fn().mockResolvedValue(response(sharing));
  vi.stubGlobal("fetch", fetch);
  render(<SharePage />);
  fireEvent.click(await screen.findByRole("button", { name: "Replace my link and code" }));
  expect(fetch).toHaveBeenCalledTimes(1);
  expect(screen.getByRole("heading", { name: "Replace your link and code?" })).toHaveFocus();
  expect(screen.getByText(/stop working for new visitors/).closest("[role=alert]")).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Keep current link" }));
  expect(screen.getByRole("button", { name: "Replace my link and code" })).toHaveFocus();
  fireEvent.click(screen.getByRole("button", { name: "Replace my link and code" }));
  fireEvent.click(screen.getByRole("button", { name: "Confirm replacement" }));
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Your sharing details are saved."));
  expect(JSON.parse(fetch.mock.calls[1][1].body).replace_code).toBe(true);
});
