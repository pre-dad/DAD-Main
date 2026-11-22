import Link from "next/link";

/** What a signed-out visitor sees in place of private account data. */
export function SignInPrompt({ children }: { children: string }) {
  return (
    <div className="callout">
      <p>You’re signed out. Sign in with an emailed single-use link, a passkey, a phone code or a password.</p>
      <Link className="button" href="/account/sign-in">
        {children}
      </Link>
    </div>
  );
}
