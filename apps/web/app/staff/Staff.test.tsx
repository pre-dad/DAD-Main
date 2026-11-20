import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import Staff from "./Staff";

vi.mock("next/form", () => ({ default: (props: React.ComponentProps<"form">) => <form {...props} /> }));

const actor = {
  id: "staff-1",
  email: "s@example.org",
  name: "Sam Support",
  role: "OP_PARTNERSHIPS",
  active: true,
  disabled: false,
  permissions: ["donor.read"],
};
const reply = (status: number, body: unknown) => ({ ok: status < 400, status, json: async () => body });
afterEach(() => vi.unstubAllGlobals());

function serve(search: ReturnType<typeof reply>) {
  const fetch = vi.fn(async (url: string) => (url.startsWith("/api/donor/staff/me") ? reply(200, actor) : search));
  vi.stubGlobal("fetch", fetch);
  return fetch;
}

it("searches by the query in the URL and links each result to its own record", async () => {
  const fetch = serve(
    reply(200, {
      donors: [{ id: "donor-1", email: "donor@example.org", public_name: null, claimed: true, disabled: false }],
      next_cursor: null,
    }),
  );
  render(<Staff q="donor@" />);
  expect(await screen.findByRole("link", { name: "donor@example.org" })).toHaveAttribute(
    "href",
    "/staff/donors/donor-1",
  );
  expect(fetch).toHaveBeenCalledWith("/api/donor/staff/donors?q=donor%40", expect.anything());
  expect(screen.getByRole("searchbox", { name: "Find donor by email" })).toHaveValue("donor@");
  expect(screen.getByRole("search")).toHaveAttribute("action", "/staff");
  expect(screen.getByRole("searchbox")).toHaveAttribute("name", "q");
});

it("does not search before a query is submitted", async () => {
  const fetch = serve(reply(200, { donors: [], next_cursor: null }));
  render(<Staff q={null} />);
  expect(await screen.findByText("Signed in as Sam Support · OP_PARTNERSHIPS")).toBeVisible();
  expect(fetch.mock.calls.some(([url]) => url.startsWith("/api/donor/staff/donors"))).toBe(false);
  expect(screen.queryByRole("heading", { name: "Donors" })).not.toBeInTheDocument();
});

it("says when nothing matches", async () => {
  serve(reply(200, { donors: [], next_cursor: null }));
  render(<Staff q="nobody" />);
  expect(await screen.findByText("No matching donors.")).toBeVisible();
});

it("lists staff accounts for an administrator without a manual refresh", async () => {
  const admin = { ...actor, role: "SUPER_ADMIN", permissions: ["staff.manage"] };
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) =>
      url.startsWith("/api/donor/staff/me")
        ? reply(200, admin)
        : url.startsWith("/api/donor/staff/accounts")
          ? reply(200, [{ ...admin, id: "staff-2", name: "Fin Ance", email: "f@example.org", role: "SUPER_ADMIN" }])
          : reply(404, {}),
    ),
  );
  render(<Staff q={null} />);
  expect(await screen.findByText(/Fin Ance · f@example.org · SUPER_ADMIN · Active/)).toBeVisible();
  expect(screen.queryByRole("button", { name: "Refresh staff accounts" })).not.toBeInTheDocument();
});

it("announces a sign-out and moves focus to signing in again", async () => {
  const fetch = vi.fn(async (url: string) =>
    url.startsWith("/api/donor/staff/me") ? reply(200, actor) : reply(200, { signed_out: true }),
  );
  vi.stubGlobal("fetch", fetch);
  render(<Staff q={null} />);
  const signOut = await screen.findByRole("button", { name: "Sign out" });
  signOut.focus();
  fireEvent.click(signOut);
  const signIn = await screen.findByRole("link", { name: "Staff sign-in" });
  expect(fetch).toHaveBeenCalledWith("/api/donor/staff/sign-out", expect.objectContaining({ method: "POST" }));
  await vi.waitFor(() => expect(signIn).toHaveFocus());
  expect(screen.getByRole("status")).toHaveTextContent("You’ve signed out of DAD staff.");
});

const admin = {
  ...actor,
  id: "staff-admin",
  name: "Ada Admin",
  role: "SUPER_ADMIN",
  permissions: ["donor.read", "staff.manage"],
};
const colleague = {
  id: "staff-2",
  email: "c@example.org",
  name: "Cy Colleague",
  role: "OP_PARTNERSHIPS",
  active: true,
  disabled: false,
  permissions: [],
};

it("announces each account action by what it did", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) =>
      url === "/api/donor/staff/me"
        ? reply(200, admin)
        : url === "/api/donor/staff/accounts"
          ? reply(200, [admin, colleague])
          : reply(200, { ...colleague, role: "SUPER_ADMIN" }),
    ),
  );
  render(<Staff q={null} />);
  fireEvent.change(await screen.findByRole("combobox", { name: "New role for Cy Colleague" }), {
    target: { value: "SUPER_ADMIN" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Confirm role change" }));
  expect(await screen.findByText("Cy Colleague’s role changed to SUPER_ADMIN.")).toHaveAttribute("role", "status");
});

it("says when staff accounts cannot be loaded", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) =>
      url === "/api/donor/staff/me" ? reply(200, admin) : reply(503, { detail: "Unavailable" }),
    ),
  );
  render(<Staff q={null} />);
  expect(
    await screen.findByText("Staff accounts could not be loaded right now. Please retry in a moment."),
  ).toHaveAttribute("role", "alert");
});

it("offers the donor money queues only with donor access", async () => {
  serve(reply(200, { donors: [], next_cursor: null }));
  const { unmount } = render(<Staff q={null} />);
  expect(await screen.findByRole("link", { name: "Failed payments" })).toHaveAttribute(
    "href",
    "/staff/failed-payments",
  );
  expect(screen.getByRole("link", { name: "Account deletions" })).toHaveAttribute("href", "/staff/deletions");
  unmount();
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => reply(200, { ...actor, permissions: ["partner.manage"] })),
  );
  render(<Staff q={null} />);
  expect(await screen.findByRole("link", { name: "Partners" })).toBeVisible();
  expect(screen.queryByRole("link", { name: "Failed payments" })).not.toBeInTheDocument();
  expect(screen.queryByRole("link", { name: "Account deletions" })).not.toBeInTheDocument();
});

it("pages donor search results by cursor and returns to the previous page", async () => {
  const donor = (id: string) => ({ id, email: `${id}@example.org`, public_name: null, claimed: true, disabled: false });
  const fetch = vi.fn(async (url: string) => {
    if (url.startsWith("/api/donor/staff/me")) return reply(200, actor);
    if (url.includes("cursor=c2")) return reply(200, { donors: [donor("page2")], next_cursor: null });
    return reply(200, { donors: [donor("page1")], next_cursor: "c2" });
  });
  vi.stubGlobal("fetch", fetch);
  render(<Staff q="a&b" />);
  expect(await screen.findByRole("link", { name: "page1@example.org" })).toBeVisible();
  expect(screen.queryByRole("button", { name: "Previous donor page" })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Next donor page" }));
  expect(await screen.findByRole("link", { name: "page2@example.org" })).toBeVisible();
  expect(fetch).toHaveBeenCalledWith("/api/donor/staff/donors?q=a%26b&cursor=c2", expect.anything());
  expect(screen.queryByRole("button", { name: "Next donor page" })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Previous donor page" }));
  expect(await screen.findByRole("link", { name: "page1@example.org" })).toBeVisible();
});

function serveAccounts(respond: (url: string, init?: RequestInit) => ReturnType<typeof reply>) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    if (url === "/api/donor/staff/me") return reply(200, admin);
    if (url === "/api/donor/staff/accounts" && !init?.method) return reply(200, [admin, colleague]);
    return respond(url, init);
  });
  vi.stubGlobal("fetch", fetch);
  return fetch;
}

const posted = (fetch: ReturnType<typeof serveAccounts>, url: string) =>
  fetch.mock.calls.find(([called, init]) => called === url && init?.method === "POST")?.[1];

it("posts each account action to its own endpoint with the right body", async () => {
  const fetch = serveAccounts((url) =>
    url.endsWith("/recovery") || url === "/api/donor/staff/accounts"
      ? reply(200, { invitation_url: "https://staff.example/activate#token=x" })
      : reply(200, {}),
  );
  render(<Staff q={null} />);
  fireEvent.click(await screen.findByRole("button", { name: "Confirm replacement invitation" }));
  expect(await screen.findByText("Replacement invitation created for Cy Colleague.")).toBeVisible();
  expect(posted(fetch, "/api/donor/staff/accounts/staff-2/recovery")).toBeDefined();
  expect(screen.getByText("https://staff.example/activate#token=x")).toBeVisible();

  fireEvent.change(screen.getByRole("combobox", { name: "New role for Cy Colleague" }), {
    target: { value: "OP_IMPACT" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Confirm role change" }));
  await screen.findByText("Cy Colleague’s role changed to OP_IMPACT.");
  expect(JSON.parse(String(posted(fetch, "/api/donor/staff/accounts/staff-2/role")?.body))).toEqual({
    role: "OP_IMPACT",
  });

  fireEvent.click(screen.getByRole("button", { name: "Confirm disable Cy Colleague" }));
  await screen.findByText("Cy Colleague disabled.");
  expect(posted(fetch, "/api/donor/staff/accounts/staff-2/disable")).toBeDefined();

  fireEvent.change(screen.getByLabelText("Name"), { target: { value: "New Person" } });
  fireEvent.change(screen.getByLabelText("Email"), { target: { value: "new@example.org" } });
  fireEvent.change(screen.getByRole("combobox", { name: "Role" }), { target: { value: "OP_GROWTH" } });
  fireEvent.click(screen.getByRole("button", { name: "Create private invitation" }));
  await screen.findByText("Invitation created.");
  expect(JSON.parse(String(posted(fetch, "/api/donor/staff/accounts")?.body))).toEqual({
    email: "new@example.org",
    name: "New Person",
    role: "OP_GROWTH",
  });
});

it("offers every staff role from the schema in both role pickers", async () => {
  serveAccounts(() => reply(200, {}));
  render(<Staff q={null} />);
  const roles = ["SUPER_ADMIN", "OP_PARTNERSHIPS", "OP_GROWTH", "OP_IMPACT"];
  for (const name of ["New role for Cy Colleague", "Role"]) {
    const select = await screen.findByRole("combobox", { name });
    expect([...select.querySelectorAll("option")].map((option) => option.textContent)).toEqual(roles);
  }
});

it("tells a staff member their session ended when an action returns 401", async () => {
  serveAccounts(() => reply(401, { detail: "Staff sign-in required" }));
  render(<Staff q={null} />);
  fireEvent.click(await screen.findByRole("button", { name: "Confirm disable Cy Colleague" }));
  expect(await screen.findByText("Your staff session ended. Sign in again.")).toBeVisible();
  expect(screen.getByRole("link", { name: "Staff sign-in" })).toHaveAttribute("href", "/staff/sign-in");
});

it("explains a 403 on an account action and keeps the page usable", async () => {
  serveAccounts(() => reply(403, { detail: "Forbidden" }));
  render(<Staff q={null} />);
  fireEvent.click(await screen.findByRole("button", { name: "Confirm disable Cy Colleague" }));
  expect(await screen.findByText("Your staff role does not permit this action.")).toHaveAttribute("role", "alert");
  expect(screen.getByRole("button", { name: "Confirm disable Cy Colleague" })).toBeEnabled();
});
