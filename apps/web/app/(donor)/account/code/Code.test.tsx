import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import CodePage from "./Code";

const sharing = {
  public_name: "Sam",
  link: "https://dad.example/r/code",
  irl_link: "https://dad.example/r/code?via=irl",
};
const response = (data: unknown) => ({ ok: true, json: async () => data });

afterEach(() => {
  vi.unstubAllGlobals();
  Reflect.deleteProperty(navigator, "wakeLock");
  Reflect.deleteProperty(document, "fullscreenElement");
  Reflect.deleteProperty(document, "exitFullscreen");
  Reflect.deleteProperty(HTMLElement.prototype, "requestFullscreen");
});

function serve(data: unknown = sharing) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) =>
      url === "/api/donor/me/wallet" ? response({ apple: false, google: false }) : response(data),
    ),
  );
}

it("shows an on-screen code with brightness instructions and no download control", async () => {
  serve();
  render(<CodePage />);
  const image = await screen.findByRole("img");
  expect(image).toHaveAttribute("src", "/api/donor/me/referral-code.svg?view=0");
  expect(screen.getByText(/Increase your screen brightness/)).toBeVisible();
  expect(screen.queryByRole("link", { name: /download/i })).not.toBeInTheDocument();
  fireEvent.error(image);
  const alert = screen.getByText(/code image could not be loaded/);
  expect(alert).toHaveAttribute("role", "alert");
  fireEvent.click(screen.getByRole("button", { name: "Retry code" }));
  expect(await screen.findByRole("img")).toHaveAttribute("src", "/api/donor/me/referral-code.svg?view=1");
});

it("keeps the screen awake while the code is shown and releases it on leaving", async () => {
  serve();
  const release = vi.fn().mockResolvedValue(undefined);
  const request = vi.fn().mockResolvedValue({ release });
  Object.defineProperty(navigator, "wakeLock", { value: { request }, configurable: true });
  const { unmount } = render(<CodePage />);
  await screen.findByRole("img");
  await waitFor(() => expect(request).toHaveBeenCalledWith("screen"));
  unmount();
  await waitFor(() => expect(release).toHaveBeenCalled());
});

it("takes the screen lock again when the page becomes visible", async () => {
  serve();
  const request = vi.fn().mockResolvedValue({ release: vi.fn().mockResolvedValue(undefined) });
  Object.defineProperty(navigator, "wakeLock", { value: { request }, configurable: true });
  render(<CodePage />);
  await screen.findByRole("img");
  await waitFor(() => expect(request).toHaveBeenCalledTimes(1));
  await act(async () => {
    document.dispatchEvent(new Event("visibilitychange"));
  });
  await waitFor(() => expect(request).toHaveBeenCalledTimes(2));
});

it("releases a lock that is granted after the page has been left", async () => {
  serve();
  const release = vi.fn().mockResolvedValue(undefined);
  let grant: (sentinel: { release: typeof release }) => void = () => {};
  const request = vi.fn(
    () =>
      new Promise((resolve) => {
        grant = resolve;
      }),
  );
  Object.defineProperty(navigator, "wakeLock", { value: { request }, configurable: true });
  const { unmount } = render(<CodePage />);
  await screen.findByRole("img");
  await waitFor(() => expect(request).toHaveBeenCalled());
  unmount();
  await act(async () => grant({ release }));
  expect(release).toHaveBeenCalled();
});

it("explains when full-screen mode is refused and keeps the code on the page", async () => {
  serve();
  Object.defineProperty(HTMLElement.prototype, "requestFullscreen", {
    value: vi.fn().mockRejectedValue(new Error("denied")),
    configurable: true,
  });
  render(<CodePage />);
  fireEvent.click(await screen.findByRole("button", { name: "Show full screen" }));
  expect(await screen.findByText(/Full-screen mode isn’t available/)).toHaveAttribute("role", "alert");
  expect(screen.getByRole("img")).toBeVisible();
});

it("asks for a public nickname before showing a code", async () => {
  serve({ public_name: null, link: null, irl_link: null });
  render(<CodePage />);
  expect(await screen.findByText(/Your code appears once you choose the public nickname/)).toBeVisible();
  expect(screen.getByRole("link", { name: "Choose your public nickname to enable sharing" })).toHaveAttribute(
    "href",
    "/account/share",
  );
  expect(screen.queryByRole("img")).not.toBeInTheDocument();
});

it("sends a signed-out visitor to sign-in instead of showing a code", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue({ ok: false, status: 401, json: async () => ({ detail: "Sign in required" }) }),
  );
  render(<CodePage />);
  expect(await screen.findByRole("link", { name: "Sign in to show your code" })).toHaveAttribute(
    "href",
    "/account/sign-in",
  );
  expect(within(document.body).queryByRole("img")).not.toBeInTheDocument();
});
