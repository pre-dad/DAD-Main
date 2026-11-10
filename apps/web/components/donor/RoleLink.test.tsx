import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { analyticsChoice } from "@/lib/analytics";
import { RoleLink } from "./RoleLink";

afterEach(() => {
  localStorage.clear();
  sessionStorage.clear();
  vi.unstubAllGlobals();
});

it("records role_selected with no properties when followed", () => {
  const fetch = vi.fn().mockResolvedValue(new Response("{}"));
  vi.stubGlobal("fetch", fetch);
  analyticsChoice(true);
  const onClick = vi.fn((event: { preventDefault(): void }) => event.preventDefault());
  render(
    <RoleLink href="/apply" onClick={onClick}>
      Become a partner
    </RoleLink>,
  );
  fireEvent.click(screen.getByRole("link", { name: "Become a partner" }));
  expect(onClick).toHaveBeenCalledTimes(1);
  expect(fetch).toHaveBeenCalledTimes(1);
  expect(JSON.parse(fetch.mock.calls[0][1].body).events[0]).toMatchObject({ name: "role_selected", properties: {} });
});
