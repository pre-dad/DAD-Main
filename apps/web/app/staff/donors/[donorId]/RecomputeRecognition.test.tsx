import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { RecomputeRecognition } from "./RecomputeRecognition";

afterEach(() => vi.unstubAllGlobals());

it("records the reason and refreshes the donor after a successful repair", async () => {
  const fetch = vi.fn(() => Promise.resolve(Response.json({ recomputed: true })));
  vi.stubGlobal("fetch", fetch);
  const onComplete = vi.fn();
  render(<RecomputeRecognition donor="donor-1" onComplete={onComplete} />);
  fireEvent.click(screen.getByText("Repair recognition after a system error"));
  fireEvent.change(screen.getByLabelText("Reason for repair"), {
    target: { value: "Repair after system error DAD-123" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Recompute recognition" }));
  await waitFor(() =>
    expect(screen.getByRole("status")).toHaveTextContent("Recognition recomputed from recorded facts"),
  );
  expect(fetch).toHaveBeenCalledWith(
    "/api/donor/staff/donors/donor-1/recognition/recompute",
    expect.objectContaining({ method: "POST", body: JSON.stringify({ reason: "Repair after system error DAD-123" }) }),
  );
  expect(onComplete).toHaveBeenCalledOnce();
});

it("retains the repair reason when authorization is refused", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(() => Promise.resolve(Response.json({ detail: "Permission denied" }, { status: 403 }))),
  );
  const onComplete = vi.fn();
  render(<RecomputeRecognition donor="donor-1" onComplete={onComplete} />);
  fireEvent.click(screen.getByText("Repair recognition after a system error"));
  fireEvent.change(screen.getByLabelText("Reason for repair"), {
    target: { value: "Repair after system error DAD-123" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Recompute recognition" }));
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("Permission denied"));
  await waitFor(() => expect(screen.getByRole("button", { name: "Recompute recognition" })).toBeEnabled());
  expect(screen.getByLabelText("Reason for repair")).toHaveValue("Repair after system error DAD-123");
  expect(onComplete).not.toHaveBeenCalled();
});

it("resets the streak through its own endpoint and button", async () => {
  const fetch = vi.fn(() => Promise.resolve(Response.json({})));
  vi.stubGlobal("fetch", fetch);
  const onComplete = vi.fn();
  render(<RecomputeRecognition donor="donor-1" onComplete={onComplete} />);
  fireEvent.click(screen.getByText("Repair recognition after a system error"));
  fireEvent.click(screen.getByLabelText(/Reset the current streak to zero/));
  fireEvent.change(screen.getByLabelText("Reason for repair"), { target: { value: "  Streak corrupted by DAD-9  " } });
  fireEvent.click(screen.getByRole("button", { name: "Reset streak" }));
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Streak reset to zero."));
  expect(fetch).toHaveBeenCalledWith(
    "/api/donor/staff/donors/donor-1/recognition/reset",
    expect.objectContaining({ body: JSON.stringify({ reason: "Streak corrupted by DAD-9" }) }),
  );
  expect(onComplete).toHaveBeenCalledWith("Streak reset to zero. The reason is in the audit log.");
});

it("rejects a reason that is only whitespace without calling the API", () => {
  const fetch = vi.fn();
  vi.stubGlobal("fetch", fetch);
  render(<RecomputeRecognition donor="donor-1" onComplete={vi.fn()} />);
  fireEvent.click(screen.getByText("Repair recognition after a system error"));
  fireEvent.change(screen.getByLabelText("Reason for repair"), { target: { value: "            " } });
  fireEvent.submit(screen.getByLabelText("Reason for repair").closest("form") as HTMLFormElement);
  expect(screen.getByRole("alert")).toHaveTextContent("at least 10 characters");
  expect(fetch).not.toHaveBeenCalled();
});

it("clears the previous success message when the next repair starts", async () => {
  vi.stubGlobal(
    "fetch",
    vi
      .fn()
      .mockResolvedValueOnce(Response.json({}))
      .mockResolvedValueOnce(Response.json({ detail: "Permission denied" }, { status: 403 })),
  );
  render(<RecomputeRecognition donor="donor-1" onComplete={vi.fn()} />);
  fireEvent.click(screen.getByText("Repair recognition after a system error"));
  const reason = screen.getByLabelText("Reason for repair");
  fireEvent.change(reason, { target: { value: "Repair after system error one" } });
  fireEvent.click(screen.getByRole("button", { name: "Recompute recognition" }));
  await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Recognition recomputed"));
  fireEvent.change(reason, { target: { value: "Repair after system error two" } });
  fireEvent.click(screen.getByRole("button", { name: "Recompute recognition" }));
  await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("Permission denied"));
  expect(screen.getByRole("status")).toBeEmptyDOMElement();
});
