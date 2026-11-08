import { fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { expect, it } from "vitest";
import { maxInterestLength, maxInterests, VolunteerInterests } from "./VolunteerInterests";

function Harness({ initial = [] as string[] }) {
  const [interests, setInterests] = useState(initial);
  return (
    <>
      <VolunteerInterests interests={interests} onChange={setInterests} />
      <div data-testid="value">{JSON.stringify(interests)}</div>
    </>
  );
}

const value = () => JSON.parse(screen.getByTestId("value").textContent ?? "[]") as string[];
const box = () => screen.getByRole("textbox", { name: "Add an interest" });
const add = (text: string) => {
  fireEvent.change(box(), { target: { value: text } });
  fireEvent.click(screen.getByRole("button", { name: "Add interest" }));
};

it("shows an empty state, adds with the button and Enter, and announces both", () => {
  render(<Harness />);
  expect(screen.getByText("No interests added yet.")).toBeVisible();
  add("  Mentoring ");
  expect(screen.getByRole("status")).toHaveTextContent("Added Mentoring. 1 of 20 interests.");
  fireEvent.change(box(), { target: { value: "Gardening" } });
  fireEvent.keyDown(box(), { key: "Enter" });
  expect(value()).toEqual(["Mentoring", "Gardening"]);
  expect(screen.queryByText("No interests added yet.")).toBeNull();
});

it("removes an interest by its accessible name and announces it", () => {
  render(<Harness initial={["Mentoring", "Gardening"]} />);
  fireEvent.click(screen.getByRole("button", { name: "Remove Mentoring" }));
  expect(value()).toEqual(["Gardening"]);
  expect(screen.getByRole("status")).toHaveTextContent("Removed Mentoring. 1 of 20 interests.");
});

it("rejects blanks and case-insensitive repeats", () => {
  render(<Harness initial={["Mentoring"]} />);
  add("   ");
  expect(screen.getByRole("alert")).toHaveTextContent("Type an interest to add.");
  add("mentoring");
  expect(screen.getByRole("alert")).toHaveTextContent("“mentoring” is already on your list.");
  expect(box()).toHaveAttribute("aria-invalid", "true");
  expect(value()).toEqual(["Mentoring"]);
});

it("enforces the length limit in characters, not code units", () => {
  render(<Harness />);
  add("😀".repeat(maxInterestLength));
  expect(value()).toHaveLength(1);
  add("a".repeat(maxInterestLength + 1));
  expect(screen.getByRole("alert")).toHaveTextContent("60 characters or fewer");
  expect(value()).toHaveLength(1);
});

it("stops at the maximum count", () => {
  render(<Harness initial={Array.from({ length: maxInterests }, (_, index) => `Interest ${index}`)} />);
  add("One more");
  expect(screen.getByRole("alert")).toHaveTextContent("up to 20 interests");
  expect(value()).toHaveLength(maxInterests);
});
