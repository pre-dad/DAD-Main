"use client";

import Link from "next/link";
import { Announcer } from "@/components/donor/Announcer";
import { Page } from "@/components/donor/Page";
import type { Schema } from "@/lib/api";
import { resourceMessages, useApiResource } from "@/lib/useApiResource";

export default function Status() {
  const { resource, data, failed, reload } = useApiResource<Schema<"ApplicationStatus">>(
    "/donor/partner/application-status",
  );
  const messages = resourceMessages(resource, {
    loading: "Checking your application…",
    failure: "Your application status could not be loaded. Please try again.",
  });
  return (
    <Page eyebrow="Partners" title="Application status">
      {resource.state === "signedOut" ? (
        <p>
          This browser has no current application receipt. Use the browser you applied from within the last 30 days. If
          you applied earlier or need help, contact the DAD partnerships team through the About page.
        </p>
      ) : (
        <Announcer status={messages.status} alert={messages.alert} />
      )}
      {data && (
        <section className="callout">
          <h2>{data.organization}</h2>
          {data.status === "PENDING" && (
            <p>Your application is pending review. We’ll email your work address when it is approved.</p>
          )}
          {data.status === "APPROVED" && (
            <p>Your application is approved. Use the sign-in link sent to your work email, or request a new link.</p>
          )}
          {data.status === "REJECTED" && (
            <p>Your application was not approved. Contact the DAD partnerships team if you need more information.</p>
          )}
        </section>
      )}
      {failed && (
        <button type="button" className="secondary" onClick={reload}>
          Try again
        </button>
      )}
      <p className="links">
        <Link href="/apply">Partner applications</Link>
        <Link href="/how-it-works">About DAD</Link>
        {data?.status === "APPROVED" && <a href="/partner/sign-in">Partner sign-in</a>}
      </p>
    </Page>
  );
}
