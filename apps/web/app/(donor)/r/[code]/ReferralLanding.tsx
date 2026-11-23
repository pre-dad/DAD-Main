"use client";

import Link from "next/link";
import { useEffect, useRef, useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { GiftFraming } from "@/components/donor/GiftFraming";
import { Page } from "@/components/donor/Page";
import { api, classifyFailure, type RequestBody, type ResponseBody } from "@/lib/api";
import { foundingDeadlineLabel, launchDateLabel } from "@/lib/launch";

export type Referrer = { state: "found"; name: string } | { state: "unavailable" } | { state: "unknown" };
type Capture = ResponseBody<"/donor/referrals/{code}", "post">;

const failures = {
  offline: "We couldn’t confirm your referral because you appear to be offline. Check your connection, then retry.",
  rateLimited:
    "We couldn’t confirm your referral: this connection has opened too many referral links. Wait a minute, then retry.",
  other: "We couldn’t confirm your referral. Please retry in a moment.",
};

export default function ReferralLanding({
  code,
  kind,
  referrer = { state: "unknown" },
}: {
  code: string;
  kind: "LINK" | "IRL";
  referrer?: Referrer;
}) {
  const [result, setResult] = useState<Capture | null>(null);
  const [error, setError] = useState("");
  const [retry, setRetry] = useState(0);
  const [leaving, setLeaving] = useState(false);
  const capture = useRef<{ pending: Promise<unknown>; signal: AbortSignal } | null>(null);
  // biome-ignore lint/correctness/useExhaustiveDependencies: retry is the retry trigger, deliberately unread
  useEffect(() => {
    const controller = new AbortController();
    setResult(null);
    setError("");
    // The capture sets the attribution cookie, so it must come from the visitor's browser.
    const pending = api<Capture>(`/donor/referrals/${encodeURIComponent(code)}`, {
      method: "POST",
      body: JSON.stringify({ kind } satisfies RequestBody<"/donor/referrals/{code}", "post">),
      signal: controller.signal,
    })
      .then((value) => {
        if (!controller.signal.aborted) setResult(value);
      })
      .catch((cause) => {
        if (controller.signal.aborted) return;
        const failure = classifyFailure(cause);
        setError(
          failure === "offline" ? failures.offline : failure === "rateLimited" ? failures.rateLimited : failures.other,
        );
      });
    capture.current = { pending, signal: controller.signal };
    return () => controller.abort();
  }, [code, kind, retry]);
  // A tap before the referral is saved waits for it (briefly), so the gift is still attributed.
  async function give(event: React.MouseEvent<HTMLAnchorElement>) {
    if (result || error || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    const current = capture.current;
    if (leaving || !current) return;
    setLeaving(true);
    let timeout: ReturnType<typeof setTimeout> | undefined;
    try {
      await Promise.race([
        current.pending,
        new Promise((resolve) => {
          timeout = setTimeout(resolve, 4000);
        }),
      ]);
    } finally {
      clearTimeout(timeout);
    }
    if (!current.signal.aborted) window.location.assign("/donate");
  }
  // Keep the server-rendered owner of the opened code; a capture response must not rename the invitation.
  const name = referrer.state === "found" ? referrer.name : null;
  const unavailable = referrer.state === "unavailable" || (referrer.state === "unknown" && result?.captured === false);

  if (unavailable)
    return (
      <Page eyebrow="Invitation" title="This referral link is no longer available">
        <p className="lead">
          The person who shared it may have replaced it with a new link. You can still give without a referral.
        </p>
        <Link className="button full" href="/donate">
          Continue without a referral
        </Link>
        <About />
      </Page>
    );

  return (
    <Page eyebrow="You’re invited" title={name ? `${name} invited you to DAD` : "Join the beginning"}>
      <p className="lead">Join a growing circle of people giving together. DAD launches {launchDateLabel()}.</p>
      <div className="callout highlight">
        <GiftFraming minor={3000} />
        <p>Monthly giving, about a dollar a day.</p>
      </div>
      <Announcer status={leaving ? "Saving your referral…" : ""} alert={error} />
      {error ? (
        <div className="actions">
          <button type="button" className="button" onClick={() => setRetry((value) => value + 1)}>
            Retry referral
          </button>
          <Link className="secondary" href="/donate">
            Continue to giving anyway
          </Link>
        </div>
      ) : (
        <Link className="button full" href="/donate" onClick={give} aria-disabled={leaving || undefined}>
          {leaving ? "Saving your referral…" : name ? `Give $1 a day with ${name}` : "Give $1 a day"}
        </Link>
      )}
      <About />
    </Page>
  );
}

function About() {
  return (
    <p>
      A first gift completed on or before {foundingDeadlineLabel()} (New York time) earns a permanent Founding Donor
      badge.
    </p>
  );
}
