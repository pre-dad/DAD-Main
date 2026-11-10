import type { Metadata } from "next";
import { GiftFraming } from "@/components/donor/GiftFraming";
import { Page } from "@/components/donor/Page";
import { foundingDeadlineLabel, launchDateLabel } from "@/lib/launch";
import { isGiftAmount } from "@/lib/money";

import { DashboardLink } from "./DashboardLink";

export const metadata: Metadata = { title: "Thank you", robots: { index: false } };

// The checkout success URL carries what the donor chose. It is display-only: anything
// malformed falls back to a thank-you that restates nothing.
function giftFrom(params: Record<string, string | string[] | undefined>): number | null {
  const { amount_minor: amount, frequency } = params;
  if (typeof amount !== "string" || !/^\d{1,9}$/.test(amount) || frequency !== "MONTHLY") return null;
  const minor = Number(amount);
  return isGiftAmount(minor) ? minor : null;
}

export default async function ThankYouPage({ searchParams }: PageProps<"/donate/thanks">) {
  const gift = giftFrom(await searchParams);
  return (
    <Page eyebrow="Your gift" title="You’re in. Welcome to the beginning.">
      {gift !== null && (
        <p>
          Your gift: <GiftFraming minor={gift} />
        </p>
      )}
      <p>
        DAD launches {launchDateLabel()}. A first gift completed on or before {foundingDeadlineLabel()} (New York time)
        earns a permanent Founding Donor badge.
      </p>
      <div className="callout highlight">
        <h2>What happens next</h2>
        <ol>
          <li>Stripe confirms your gift, then we email your receipt to your account’s address.</li>
          <li>
            Your <DashboardLink>dashboard</DashboardLink> holds your giving streak and your personal referral link and
            code, so the people you invite count toward your circle.
          </li>
          <li>
            When you’re ready, <a href="/account/share">invite someone to give with you</a>.
          </li>
        </ol>
      </div>
      <p className="note">
        Your receipt email follows once Stripe confirms the payment. In this sandbox preview it is a test gift record,
        not a tax receipt, and no real donation is collected.
      </p>
    </Page>
  );
}
