import Link from "next/link";
import { FoundingLine } from "@/components/donor/FoundingLine";
import { GiftFraming } from "@/components/donor/GiftFraming";
import { RoleLink } from "@/components/donor/RoleLink";
import { launchDateLabel, launchMonthLabel } from "@/lib/launch";

const steps = [
  { number: "01", title: "Create your account", detail: "Verify your email with one tap." },
  { number: "02", title: "Give", detail: "Choose a monthly gift and pay on Stripe’s secure page." },
  { number: "03", title: "Invite someone", detail: "Share your link or show your code in person." },
];

export default function DonorHome() {
  return (
    <>
      <section className="hero">
        <div className="heroCopy">
          <span className="eyebrow">Aid, delivered.</span>
          <h1>A dollar a day. Every dollar on the record.</h1>
          <p>Verify your email and give monthly. Then invite the people you know and watch your circle grow.</p>
          <div className="actions">
            <RoleLink className="button" href="/donate">
              Give $1 a day{" "}
              <span aria-hidden="true" className="arrow">
                →
              </span>
            </RoleLink>
            <Link className="secondary" href="/how-it-works">
              See how it works
            </Link>
          </div>
          <div>
            <GiftFraming minor={3000} />
          </div>
          <FoundingLine />
        </div>
        <aside className="heroFacts" aria-label="DAD at a glance">
          <dl>
            <div>
              <dt>Your gift</dt>
              <dd>
                <GiftFraming minor={3000} />
              </dd>
            </div>
            <div>
              <dt>Where it goes</dt>
              <dd>Spend by category, published</dd>
            </div>
            <div>
              <dt>Launch</dt>
              <dd>{launchDateLabel()}</dd>
            </div>
          </dl>
        </aside>
      </section>
      <div className="page">
        <section className="panel">
          <h2>One generous invitation at a time</h2>
          <ol className="metrics steps">
            {steps.map((step) => (
              <li className="metric" key={step.number}>
                <span className="stepNumber">{step.number}</span>
                <h3>{step.title}</h3>
                <p>{step.detail}</p>
              </li>
            ))}
          </ol>
          <p>
            Only the people you personally invite count toward your circle. A referral counts when their first gift
            completes.
          </p>
          <p className="note">
            Run an aid organization?{" "}
            <RoleLink className="quiet" href="/apply">
              Become a partner{" "}
              <span aria-hidden="true" className="arrow">
                →
              </span>
            </RoleLink>
          </p>
        </section>
        <section className="panel">
          <h2>Building toward {launchMonthLabel()}</h2>
          <p>
            DAD assembles aid initiatives with partner organizations and board approval. Programs begin at launch. No
            recipient receives money through this donor release.
          </p>
          <nav aria-label="Explore" className="links">
            <Link href="/initiatives">Explore planned initiatives</Link>
            <Link href="/spend">See spend by category</Link>
          </nav>
        </section>
      </div>
    </>
  );
}
