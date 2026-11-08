import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import Pending from "./Pending";

afterEach(() => vi.unstubAllGlobals());

const application = {
  id: "app-1",
  organization: "Delta Inc",
  website: "https://delta.example",
  type: "Corporate",
  contact_name: "Dana",
  title: "Head of Giving",
  work_email: "dana@delta.example",
  phone: "+14155550123",
  workforce_size: 500,
  status: "PENDING",
  decision_reason: "",
  approved_partner_id: null,
};

it("lists a pending application and approves it", async () => {
  const fetch = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (url.endsWith("/pending") && init?.method !== "POST") {
      return Promise.resolve(Response.json({ applications: [application], next_cursor: null }));
    }
    return Promise.resolve(Response.json({ partner_id: "p1" }));
  });
  vi.stubGlobal("fetch", fetch);
  render(<Pending />);
  await waitFor(() => expect(screen.getByRole("heading", { name: "Delta Inc" })).toBeInTheDocument());
  fireEvent.click(screen.getByRole("button", { name: "Approve" }));
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Approved Delta Inc"));
  expect(fetch.mock.calls.some(([u, i]) => String(u).includes("/app-1/approve") && i?.method === "POST")).toBe(true);
});

it("pages the approval queue past the first fifty by cursor", async () => {
  const fetch = vi.fn((input: RequestInfo | URL) =>
    Promise.resolve(
      Response.json(
        String(input).includes("cursor=c2")
          ? { applications: [{ ...application, id: "app-51", organization: "Late Co" }], next_cursor: null }
          : { applications: [application], next_cursor: "c2" },
      ),
    ),
  );
  vi.stubGlobal("fetch", fetch);
  render(<Pending />);
  fireEvent.click(await screen.findByRole("button", { name: "Later applications" }));
  expect(await screen.findByRole("heading", { name: "Late Co" })).toBeVisible();
  expect(fetch).toHaveBeenLastCalledWith("/api/donor/staff/partners/pending?cursor=c2", expect.anything());
  expect(screen.queryByRole("button", { name: "Later applications" })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Earlier applications" }));
  expect(await screen.findByRole("heading", { name: "Delta Inc" })).toBeVisible();
});
