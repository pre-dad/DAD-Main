import { render, screen } from "@testing-library/react";
import { expect, it } from "vitest";
import { PreviewBanner } from "@/components/donor/PreviewBanner";
import About from "../about/page";
import HowItWorks from "./page";

it("explains the donor release, monthly billing, direct referral credit, and launch timing", () => {
  render(<HowItWorks />);
  expect(screen.getByText(/April 22, 2027/)).toHaveTextContent("Founding Donor badge");
  expect(screen.getByText(/April 22, 2027/)).toHaveTextContent("by December 31, 2026 (New York time)");
  expect(screen.getByText(/about a dollar a day/)).toHaveTextContent(
    "$30/month. You can also choose $90, $150, or any amount up to $900/month",
  );
  expect(screen.getByText(/only people you personally refer/)).toHaveTextContent("less successful refunds");
  expect(screen.getByText(/streak growing in local calendar days/)).toHaveTextContent("7 days to recover");
  expect(screen.getByText(/streak growing in local calendar days/)).toHaveTextContent(
    "Cancellation ends it at the first skipped renewal",
  );
  expect(screen.getByText(/does not enrol recipients/)).toHaveTextContent("Aid programs begin at launch");
  expect(screen.getByText(/does not enrol recipients/)).toHaveTextContent("approved partners use the partner portal");
  expect(screen.getByText(/does not enrol recipients/)).toHaveTextContent(
    "Donor apps for iOS and Android are being developed separately",
  );
  expect(screen.queryByText(/R2/)).not.toBeInTheDocument();
  expect(screen.getByText(/approved by the board/)).toHaveTextContent("From launch, programs invite");
  expect(screen.queryByText(/sandbox preview/)).not.toBeInTheDocument();
});

it("links the explanation to giving, spend by category, and partner interest", () => {
  render(<HowItWorks />);
  expect(screen.getByRole("link", { name: "Give $1 a day" })).toHaveAttribute("href", "/donate");
  expect(screen.getByText("$30/month")).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "See spend by category" })).toHaveAttribute("href", "/spend");
  expect(screen.getByRole("link", { name: "Partner application" })).toHaveAttribute("href", "/apply");
  expect(screen.getByRole("link", { name: "Partner sign-in" })).toHaveAttribute("href", "/partner/sign-in");
  expect(screen.getByText(/totals by category/)).toHaveTextContent("never as individual transactions");
});

it("leaves the test-payment caveat to the preview banner that frames every page", () => {
  render(<PreviewBanner />);
  expect(screen.getByRole("complementary", { name: "Preview notice" })).toHaveTextContent(
    "Test payments only. No real money moves.",
  );
});
it("keeps /about as a permanent alias of the single About / how it works screen", () => {
  expect(() => About()).toThrow("NEXT_REDIRECT");
});
