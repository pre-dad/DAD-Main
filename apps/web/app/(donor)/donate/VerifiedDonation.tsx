"use client";

import { Announcer } from "@/components/donor/Announcer";
import { Page } from "@/components/donor/Page";
import { SignInForm } from "@/components/donor/SignInForm";
import type { Schema } from "@/lib/api";
import { resourceMessages, useApiResource } from "@/lib/useApiResource";
import Donate from "./Donate";

export default function VerifiedDonation() {
  const { resource, data, reload } = useApiResource<Schema<"Profile">>("/donor/me/profile");
  if (data) return <Donate key={data.email} verifiedEmail={data.email} onSignedOut={reload} />;
  const messages = resourceMessages(resource, {
    loading: "Checking your sign-in…",
    failure: "We could not check your sign-in. Please try again.",
  });
  return (
    <Page eyebrow="Give" title="Verify your email before giving">
      <p className="lead">Sign in to keep your giving and receipts private.</p>
      {resource.state === "signedOut" ? (
        <>
          <SignInForm next="/donate" />
          <p>
            New to DAD? <a href="/account/signup">Create your donor account</a>, then return here to give.
          </p>
          <p>
            Already have a password or passkey? <a href="/account/sign-in">Choose another sign-in method</a>.
          </p>
        </>
      ) : (
        <>
          <Announcer status={messages.status} alert={messages.alert} />
          {resource.state !== "loading" && (
            <button type="button" onClick={reload}>
              Try again
            </button>
          )}
        </>
      )}
    </Page>
  );
}
