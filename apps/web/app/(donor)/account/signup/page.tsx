"use client";
import { useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { Page } from "@/components/donor/Page";
import { api, failureMessage } from "@/lib/api";
export default function Signup() {
  const [busy, setBusy] = useState(false),
    [status, setStatus] = useState(""),
    [error, setError] = useState("");
  return (
    <Page eyebrow="Donor" title="Create your donor account">
      <p>Verify your email, then choose a password or another sign-in method in your profile.</p>
      <form
        onSubmit={async (e) => {
          e.preventDefault();
          if (busy) return;
          const f = new FormData(e.currentTarget);
          setBusy(true);
          setError("");
          try {
            await api("/donor/signup", { method: "POST", body: JSON.stringify(Object.fromEntries(f)) });
            setStatus("Check your email for a verification link. You can choose a password after opening it.");
          } catch (c) {
            setError(failureMessage(c, "Your account could not be started."));
          } finally {
            setBusy(false);
          }
        }}
      >
        <label>
          Name
          <input name="name" autoComplete="name" maxLength={150} required />
        </label>
        <label>
          Email
          <input name="email" type="email" autoComplete="email" required />
        </label>
        <label>
          ZIP code
          <input name="zip" autoComplete="postal-code" pattern="[0-9]{5}(-[0-9]{4})?" required />
        </label>
        <label>
          City
          <input name="city" autoComplete="address-level2" maxLength={80} required />
        </label>
        <button className="button full" type="submit" disabled={busy}>
          Verify my email
        </button>
        <Announcer status={status} alert={error} />
      </form>
      <p>
        <a href="/account/oauth/apple">Continue with Apple</a> ·{" "}
        <a href="/account/oauth/google">Continue with Google</a>
      </p>
    </Page>
  );
}
