import { act, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { useKeysetResource } from "./useKeysetResource";

afterEach(() => vi.unstubAllGlobals());
const page = (n: number, next: string | null) => ({
  ok: true,
  status: 200,
  json: async () => ({ n, next_cursor: next }),
});

function List() {
  const { data, heading, older, newer } = useKeysetResource<{ n: number; next_cursor: string | null }>(
    (cursor) => `/items${cursor ? `?cursor=${cursor}` : ""}`,
  );
  return (
    <>
      <h2 ref={heading} tabIndex={-1}>
        Items
      </h2>
      <p>page {data?.n}</p>
      {older && (
        <button type="button" onClick={older}>
          Older
        </button>
      )}
      {newer && (
        <button type="button" onClick={newer}>
          Newer
        </button>
      )}
    </>
  );
}

it("pages with cursors and focuses the heading once the new page replaces the old", async () => {
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(page(1, "c2"))
    .mockResolvedValueOnce(page(2, null))
    .mockResolvedValueOnce(page(1, "c2"));
  vi.stubGlobal("fetch", fetch);
  render(<List />);
  await screen.findByText("page 1");
  expect(screen.queryByRole("button", { name: "Newer" })).toBeNull();
  await act(async () => screen.getByRole("button", { name: "Older" }).click());
  await screen.findByText("page 2");
  expect(fetch.mock.calls[1][0]).toContain("cursor=c2");
  await waitFor(() => expect(screen.getByRole("heading")).toHaveFocus());
  expect(screen.queryByRole("button", { name: "Older" })).toBeNull();
  await act(async () => screen.getByRole("button", { name: "Newer" }).click());
  await screen.findByText("page 1");
  expect(fetch.mock.calls[2][0]).not.toContain("cursor");
});
