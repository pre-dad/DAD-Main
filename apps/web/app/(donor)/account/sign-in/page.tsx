import type { Metadata } from "next";
import { Page } from "@/components/donor/Page";
import { PasskeySignIn } from "@/components/donor/PasskeySignIn";
import { PasswordAccess } from "@/components/donor/PasswordAccess";
import { PhoneSignIn } from "@/components/donor/PhoneSignIn";
import { SignInForm } from "@/components/donor/SignInForm";
import { AlreadySignedIn } from "./AlreadySignedIn";

export const metadata: Metadata = { title: "Claim your account or sign in" };

export default function SignInPage() {
  return (
    <Page eyebrow="My account" title="Your donor account">
      <AlreadySignedIn />
      <p className="lead">Use your verified donor email. We’ll email a single-use sign-in link.</p>
      <SignInForm />
      <p className="note">The link expires after 15 minutes. You don’t need a password.</p>
      <PasskeySignIn />
      <PhoneSignIn />
      <PasswordAccess />
      <p>
        <a href="/account/oauth/apple">Sign in with Apple</a> · <a href="/account/oauth/google">Sign in with Google</a>
      </p>
      <div className="callout">
        <h2>New to DAD?</h2>
        <p>Create your donor account and verify your email before starting your first gift.</p>
        <a className="secondary" href="/account/signup">
          Create your donor account
        </a>
      </div>
    </Page>
  );
}
