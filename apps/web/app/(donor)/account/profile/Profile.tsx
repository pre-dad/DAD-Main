"use client";

import Link from "next/link";
import { useCallback, useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { Page } from "@/components/donor/Page";
import { PasswordAccess } from "@/components/donor/PasswordAccess";
import { SignInPrompt } from "@/components/donor/SignInPrompt";
import { ApiError, api, failureMessage, type RequestBody, type Schema } from "@/lib/api";
import { resourceMessages, useApiResource } from "@/lib/useApiResource";
import { Passkeys } from "./Passkeys";
import { Sessions } from "./Sessions";
import { VolunteerInterests } from "./VolunteerInterests";

const field = (form: FormData, name: string) => String(form.get(name) ?? "").trim();

export default function Profile() {
  const [signedOut, setSignedOut] = useState(false);
  const { resource, data, failed, reload, setData } = useApiResource<Schema<"Profile">>(
    signedOut ? null : "/donor/me/profile",
  );
  const [error, setError] = useState("");
  const [message, setMessage] = useState("");
  const [busy, setBusy] = useState(false);
  const [newEmail, setNewEmail] = useState("");
  const [deleted, setDeleted] = useState(false);
  const markSignedOut = useCallback(() => setSignedOut(true), []);
  const messages = resourceMessages(resource, {
    loading: "Loading profile…",
    failure: "Your profile could not be loaded right now. Please retry in a moment.",
  });
  async function run(work: () => Promise<void>, fallback: string) {
    if (busy) return;
    setBusy(true);
    setError("");
    setMessage("");
    try {
      await work();
    } catch (cause) {
      if (cause instanceof ApiError && cause.status === 401) setSignedOut(true);
      else setError(failureMessage(cause, fallback));
    } finally {
      setBusy(false);
    }
  }
  const save = (event: React.FormEvent) => {
    event.preventDefault();
    if (!data) return;
    run(async () => {
      setData(
        await api<Schema<"Profile">>("/donor/me/preferences", {
          method: "POST",
          body: JSON.stringify({
            version: data.version,
            referral_emails: data.referral_emails,
            marketing_emails: data.marketing_emails,
            push_referral_converted: data.push_referral_converted,
            push_streak_at_risk: data.push_streak_at_risk,
            push_streak_milestone: data.push_streak_milestone,
            push_streak_lapsed: data.push_streak_lapsed,
            push_badge_earned: data.push_badge_earned,
            push_gift_processed: data.push_gift_processed,
            push_event_nearby: data.push_event_nearby,
            push_event_reminder: data.push_event_reminder,
            push_event_promoted: data.push_event_promoted,
            rankings_opted_out: data.rankings_opted_out,
            network_visible: data.network_visible,
            volunteer: data.volunteer,
            volunteer_interests: data.volunteer_interests,
          } satisfies RequestBody<"/donor/me/preferences", "post">),
        }),
      );
      setMessage("Preferences saved.");
    }, "Preferences were not confirmed. Refresh to check the saved choices before retrying.");
  };
  const saveDetails = (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    run(async () => {
      setData(
        await api<Schema<"Profile">>("/donor/me/profile", {
          method: "POST",
          body: JSON.stringify({
            zip: field(form, "zip"),
            city: field(form, "city"),
            phone: field(form, "phone"),
          } satisfies RequestBody<"/donor/me/profile", "post">),
        }),
      );
      setMessage("Your details were saved.");
    }, "Your details were not saved. Please retry in a moment.");
  };
  const deleteAccount = () => {
    if (!window.confirm("Delete your DAD account? This can’t be undone.")) return;
    run(async () => {
      await api("/donor/me/delete", { method: "POST" });
      setDeleted(true);
    }, "Your account was not deleted. Please retry in a moment.");
  };
  const changeEmail = (event: React.FormEvent) => {
    event.preventDefault();
    run(async () => {
      await api("/donor/me/email-change", {
        method: "POST",
        body: JSON.stringify({ email: newEmail } satisfies RequestBody<"/donor/me/email-change", "post">),
      });
      setMessage(
        "Check your current email address first. After you confirm there, we’ll send a second confirmation to your new address. Each link expires after 15 minutes.",
      );
      setNewEmail("");
    }, "We couldn’t request the change right now. Please retry in a moment.");
  };
  const logout = () =>
    run(async () => {
      await api("/donor/logout", { method: "POST" });
      setSignedOut(true);
    }, "Sign-out was not confirmed. Please retry.");
  const logoutAll = () =>
    run(async () => {
      await api("/donor/logout-all", { method: "POST" });
      setSignedOut(true);
    }, "Sign-out was not confirmed. Please retry.");
  return (
    <Page
      eyebrow="My account"
      title="Profile and notifications"
      back={{ href: "/account/dashboard", label: "My dashboard" }}
    >
      {deleted ? (
        <p role="status">
          Your account has been deleted. Your personal details are gone; the gift records the law requires are kept
          without them.
        </p>
      ) : signedOut || resource.state === "signedOut" ? (
        <SignInPrompt>Sign in to manage your profile</SignInPrompt>
      ) : (
        <>
          {data && (
            <>
              <ul className="rows">
                <li>
                  <span>Account email</span>
                  <strong>{data.email}</strong>
                </li>
                <li>
                  <span>Public nickname</span>
                  <span>
                    <strong>{data.public_name || "Not shared"}</strong> · <Link href="/account/share">Manage</Link>
                  </span>
                </li>
              </ul>
              <form onSubmit={saveDetails} key={`${data.zip}-${data.city}-${data.phone}`}>
                <fieldset disabled={busy}>
                  <legend>Your details</legend>
                  <label>
                    ZIP code
                    <input
                      name="zip"
                      inputMode="numeric"
                      autoComplete="postal-code"
                      pattern="[0-9]{5}(-[0-9]{4})?"
                      maxLength={10}
                      defaultValue={data.zip ?? ""}
                    />
                  </label>
                  <label>
                    City
                    <input name="city" autoComplete="address-level2" maxLength={80} defaultValue={data.city ?? ""} />
                  </label>
                  <label>
                    Phone
                    <input
                      name="phone"
                      type="tel"
                      autoComplete="tel"
                      placeholder="+14155550123"
                      pattern="\+[1-9][0-9]{1,14}"
                      defaultValue={data.phone ?? ""}
                    />
                  </label>
                  <button className="secondary" type="submit">
                    Save details
                  </button>
                </fieldset>
              </form>
              <p className="note">Your ZIP code sets your city for events and rankings, and your time zone.</p>
              <form onSubmit={save}>
                <fieldset disabled={busy}>
                  <legend>Optional email notifications</legend>
                  <label className="choice">
                    <input
                      type="checkbox"
                      checked={data.referral_emails}
                      onChange={(event) => setData({ ...data, referral_emails: event.target.checked })}
                    />{" "}
                    Tell me when a Link or IRL Referral gives for the first time
                  </label>
                  <label className="choice">
                    <input
                      type="checkbox"
                      checked={data.marketing_emails}
                      onChange={(event) => setData({ ...data, marketing_emails: event.target.checked })}
                    />{" "}
                    Send me DAD news and campaign updates
                  </label>
                </fieldset>
                <p className="note">
                  These choices are optional and start off. Gift records, account access, and essential payment notices
                  are separate.
                </p>
                <fieldset disabled={busy}>
                  <legend>DAD app notifications</legend>
                  <label className="choice">
                    <input
                      type="checkbox"
                      checked={data.push_referral_converted}
                      onChange={(event) => setData({ ...data, push_referral_converted: event.target.checked })}
                    />{" "}
                    When someone I invited gives for the first time
                  </label>
                  <label className="choice">
                    <input
                      type="checkbox"
                      checked={data.push_streak_at_risk}
                      onChange={(event) => setData({ ...data, push_streak_at_risk: event.target.checked })}
                    />{" "}
                    When a declined payment puts my giving streak at risk
                  </label>
                  <label className="choice">
                    <input
                      type="checkbox"
                      checked={data.push_streak_milestone}
                      onChange={(event) => setData({ ...data, push_streak_milestone: event.target.checked })}
                    />{" "}
                    When my giving streak reaches a milestone
                  </label>
                  <label className="choice">
                    <input
                      type="checkbox"
                      checked={data.push_streak_lapsed}
                      onChange={(event) => setData({ ...data, push_streak_lapsed: event.target.checked })}
                    />{" "}
                    When my giving streak ends
                  </label>
                  <label className="choice">
                    <input
                      type="checkbox"
                      checked={data.push_gift_processed}
                      onChange={(event) => setData({ ...data, push_gift_processed: event.target.checked })}
                    />{" "}
                    When my gift is processed
                  </label>
                  <label className="choice">
                    <input
                      type="checkbox"
                      checked={data.push_badge_earned}
                      onChange={(event) => setData({ ...data, push_badge_earned: event.target.checked })}
                    />{" "}
                    When I earn a badge
                  </label>
                  <label className="choice">
                    <input
                      type="checkbox"
                      checked={data.push_event_reminder}
                      onChange={(event) => setData({ ...data, push_event_reminder: event.target.checked })}
                    />{" "}
                    Before an event I signed up for
                  </label>
                  <label className="choice">
                    <input
                      type="checkbox"
                      checked={data.push_event_promoted}
                      onChange={(event) => setData({ ...data, push_event_promoted: event.target.checked })}
                    />{" "}
                    When a waitlist spot opens for me
                  </label>
                  {data.volunteer && (
                    <label className="choice">
                      <input
                        type="checkbox"
                        checked={data.push_event_nearby}
                        onChange={(event) => setData({ ...data, push_event_nearby: event.target.checked })}
                      />{" "}
                      When a volunteer event is announced near me
                    </label>
                  )}
                </fieldset>
                <p className="note">
                  App notifications reach only phones where you signed in to the DAD app and allowed its notifications.
                  They start on and never show names or amounts.
                </p>
                <fieldset disabled={busy}>
                  <legend>Rankings and network</legend>
                  <label className="choice">
                    <input
                      type="checkbox"
                      checked={!data.rankings_opted_out}
                      onChange={(event) => setData({ ...data, rankings_opted_out: !event.target.checked })}
                    />{" "}
                    Rank my giving streak nationally and in my city
                  </label>
                  <label className="choice">
                    <input
                      type="checkbox"
                      checked={data.network_visible}
                      onChange={(event) => setData({ ...data, network_visible: event.target.checked })}
                    />{" "}
                    Show my public nickname to the person who invited me
                  </label>
                </fieldset>
                <p className="note">
                  Only you see your own rank; no one sees a list of names. When hidden, you’re counted in your inviter’s
                  network without your nickname.
                </p>
                <fieldset disabled={busy}>
                  <legend>Volunteering</legend>
                  <label className="choice">
                    <input
                      type="checkbox"
                      checked={data.volunteer}
                      onChange={(event) => setData({ ...data, volunteer: event.target.checked })}
                    />{" "}
                    I volunteer
                  </label>
                  {data.volunteer && (
                    <VolunteerInterests
                      interests={data.volunteer_interests}
                      onChange={(volunteer_interests) => setData({ ...data, volunteer_interests })}
                      disabled={busy}
                    />
                  )}
                </fieldset>
                <button className="button" disabled={busy} type="submit">
                  {busy ? "Saving…" : "Save preferences"}
                </button>
              </form>
              <details>
                <summary>Change account email</summary>
                <p>
                  You must confirm both addresses. Completing the change signs out every device and removes your
                  passkeys. Existing accounts will not be merged.
                </p>
                <form onSubmit={changeEmail}>
                  <label>
                    New email address
                    <input
                      type="email"
                      autoComplete="email"
                      maxLength={320}
                      required
                      value={newEmail}
                      onChange={(event) => setNewEmail(event.target.value)}
                      disabled={busy}
                    />
                  </label>
                  <button type="submit" className="secondary" disabled={busy}>
                    Send confirmation to current address
                  </button>
                </form>
              </details>
              <PasswordAccess setup />
              <Passkeys onSignedOut={markSignedOut} />
              <Sessions onSignedOut={markSignedOut} />
              <button type="button" className="secondary" disabled={busy} onClick={logout}>
                Sign out of this device
              </button>
              <details>
                <summary>Sign out on every device</summary>
                <p>
                  This ends all existing donor sessions, including this one. You can sign in again with a new email
                  link. Your passkeys are kept, so remove any you don’t recognize under Passkeys first.
                </p>
                <button type="button" className="secondary danger" disabled={busy} onClick={logoutAll}>
                  Confirm sign out everywhere
                </button>
              </details>
              <details>
                <summary>Delete account</summary>
                <p>
                  Deleting your account removes your personal details, passkeys, sessions and preferences, and gives up
                  your places at upcoming events. Gift records the law requires are kept without your details. If your
                  monthly gift is still set to renew, <Link href="/account/recurring">cancel it</Link> first.
                </p>
                <button type="button" className="secondary danger" disabled={busy} onClick={deleteAccount}>
                  Delete my account
                </button>
              </details>
            </>
          )}
          <Announcer status={messages.status || message} alert={error || messages.alert} />
          {(failed || error) && (
            <button type="button" className="secondary" disabled={busy} onClick={reload}>
              Refresh profile
            </button>
          )}
        </>
      )}
    </Page>
  );
}
