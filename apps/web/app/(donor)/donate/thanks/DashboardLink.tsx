"use client";
import { useEffect } from "react";

// Donate.tsx owns this key; it holds the choice for the Stripe-cancel return.
const CHOICE_KEY = "dad.donate.choice";

/** Links to the dashboard and clears the remembered choice, which is no longer needed once the gift is made. */
export function DashboardLink({ children }: { children: React.ReactNode }) {
  useEffect(() => {
    try {
      sessionStorage.removeItem(CHOICE_KEY);
    } catch {
      // Storage can be blocked; the choice then expires with the tab.
    }
  }, []);
  // A full page load: account pages need their own per-request security policy.
  return <a href="/account/dashboard">{children}</a>;
}
