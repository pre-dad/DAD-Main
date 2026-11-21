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
  fireEvent.click(screen.getByRole("button", { name: "Confirm approval of Delta Inc" }));
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

function queue(onDecide: () => Response) {
  const fetch = vi.fn((_input: RequestInfo | URL, init?: RequestInit) =>
    Promise.resolve(
      init?.method === "POST" ? onDecide() : Response.json({ applications: [application], next_cursor: null }),
    ),
  );
  vi.stubGlobal("fetch", fetch);
  return fetch;
}

it("rejects with the reason typed in the form, trimmed", async () => {
  const fetch = queue(() => Response.json({}));
  render(<Pending />);
  const reject = await screen.findByRole("button", { name: "Confirm rejection of Delta Inc" });
  expect(reject).toBeDisabled();
  fireEvent.change(screen.getByLabelText("Reason for rejecting Delta Inc"), { target: { value: "  Not a fit  " } });
  fireEvent.click(reject);
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Rejected Delta Inc."));
  expect(fetch).toHaveBeenCalledWith(
    "/api/donor/staff/partners/app-1/reject",
    expect.objectContaining({ method: "POST", body: JSON.stringify({ reason: "Not a fit" }) }),
  );
});

it("will not reject on a whitespace-only reason", async () => {
  const fetch = queue(() => Response.json({}));
  render(<Pending />);
  fireEvent.change(await screen.findByLabelText("Reason for rejecting Delta Inc"), { target: { value: "   " } });
  expect(screen.getByRole("button", { name: "Confirm rejection of Delta Inc" })).toBeDisabled();
  expect(fetch.mock.calls.some(([, init]) => init?.method === "POST")).toBe(false);
});

it("shows the server's reason when a decision conflicts and reloads the queue", async () => {
  const fetch = queue(() => Response.json({ detail: "That work email already has a login." }, { status: 409 }));
  render(<Pending />);
  fireEvent.click(await screen.findByRole("button", { name: "Confirm approval of Delta Inc" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("That work email already has a login.");
  await waitFor(() => expect(fetch.mock.calls.filter(([, init]) => init?.method !== "POST")).toHaveLength(2));
});

it("falls back to a plain message when the decision cannot reach the server", async () => {
  const fetch = vi.fn((_input: RequestInfo | URL, init?: RequestInit) =>
    init?.method === "POST"
      ? Promise.reject(new TypeError("Failed to fetch"))
      : Promise.resolve(Response.json({ applications: [application], next_cursor: null })),
  );
  vi.stubGlobal("fetch", fetch);
  render(<Pending />);
  fireEvent.click(await screen.findByRole("button", { name: "Confirm approval of Delta Inc" }));
  expect(await screen.findByRole("alert")).toHaveTextContent(/offline/i);
});

it("announces a load failure and retries", async () => {
  vi.stubGlobal(
    "fetch",
    vi
      .fn()
      .mockResolvedValueOnce(Response.json({ detail: "boom" }, { status: 500 }))
      .mockResolvedValue(Response.json({ applications: [application], next_cursor: null })),
  );
  render(<Pending />);
  expect(await screen.findByRole("alert")).toHaveTextContent("Pending applications could not be loaded right now");
  fireEvent.click(screen.getByRole("button", { name: "Retry loading" }));
  expect(await screen.findByRole("heading", { name: "Delta Inc" })).toBeVisible();
});
