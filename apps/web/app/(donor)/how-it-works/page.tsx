import type { Metadata } from "next";
import Link from "next/link";
import { GiftFraming } from "@/components/donor/GiftFraming";
import { Page } from "@/components/donor/Page";
import { foundingDeadlineLabel, launchDateLabel, launchMonthLabel } from "@/lib/launch";

export const metadata: Metadata = {
  title: "About DAD and how it works",
  description: `How giving, referrals, streaks, and the Founding Donor badge work, and what DAD’s ${launchMonthLabel()} launch brings.`,
};

export default function HowItWorks() {
  return (
    <Page eyebrow="How it works" title="A small gift. A growing circle.">
      <p className="lead">
        DAD brings donors and partner organizations together to support practical aid work. Each initiative is assembled
        with a partner and approved by the board. From launch, programs invite people to enrol in that work.
      </p>
      <h2>Giving</h2>
      <p>
        Create a donor account and verify your email, then give monthly on the web. Giving suggests a dollar a day,
        billed as $30/month. You can give more in whole dollars a day, up to $30 a day ($900/month); the monthly charge
        is always the daily amount × 30.
      </p>
      <h2>Your circle</h2>
      <p>
        Your account holds your personal link and a referral code to show on your phone. A Link
        Referral begins with a shared link; an IRL Referral begins when someone scans your code. Your dashboard counts
        them separately, once a first gift completes.
      </p>
      <p>
        Your circle includes only people you personally refer. Attributed dollars reflect money actually collected, less
        successful refunds.
      </p>
      <h2>Streaks and recognition</h2>
      <p>
        Successful monthly gifts keep your streak growing in local calendar days. A failed renewal has 7 days to recover
        before your active streak resets. Cancellation ends it at the first skipped renewal. Your historical best stays
        recorded.
      </p>
      <p>
        DAD launches on {launchDateLabel()}. A first gift completed by {foundingDeadlineLabel()} (New York time) earns a
        permanent Founding Donor badge.
      </p>
      <h2>Clear records, honest limits</h2>
      <p>
        DAD publishes spend as totals by category, never as individual transactions. Staff verify each figure against
        finance records before it is published.
      </p>
      <h2>Programs start at launch</h2>
      <p>
        This donor release does not enrol recipients or send money to them. Aid programs begin at launch. Partners can
        apply now; approved partners use the partner portal to manage their profile, initiatives, employee giving, and
        reports. Donor apps for iOS and Android are being developed separately.
      </p>
      <div className="actions">
        <Link className="button" href="/donate">
          Give $1 a day
        </Link>
        <GiftFraming minor={3000} />
        <Link className="secondary" href="/spend">
          See spend by category
        </Link>
      </div>
      <p>
        <Link href="/apply">Partner application</Link>
        {" · "}
        <a href="/partner/sign-in">Partner sign-in</a>
      </p>
    </Page>
  );
}
