"use client";
import { useEffect } from "react";

// Donate.tsx owns this key; it holds email, name, ZIP and city for the Stripe-cancel return.
const CHOICE_KEY = "dad.donate.choice";
/** Read by SignInForm, which removes it. Only the email crosses; never a URL. */
const SIGNIN_EMAIL_KEY = "dad.signin.email";
/**
 * Hands the donor's email to the sign-in page through this tab's sessionStorage, then
 * clears the fuller remembered choice, which is no longer needed once the gift is made.
 */
export function ResendLink({ children }: { children: React.ReactNode }) {
  useEffect(() => {
    try {
      const choice = JSON.parse(sessionStorage.getItem(CHOICE_KEY) ?? "null");
      if (typeof choice?.email === "string" && choice.email) sessionStorage.setItem(SIGNIN_EMAIL_KEY, choice.email);
      sessionStorage.removeItem(CHOICE_KEY);
    } catch {
      // Storage can be blocked; the donor simply types the address.
    }
  }, []);
  // A full page load: account pages need their own per-request security policy.
  return <a href="/account/sign-in">{children}</a>;
}
