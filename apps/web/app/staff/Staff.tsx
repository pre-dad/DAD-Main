"use client";
import Form from "next/form";
import Link from "next/link";
import { useEffect, useRef, useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { Page } from "@/components/donor/Page";
import { ApiError, api, failureMessage, type ResponseBody, type Schema } from "@/lib/api";
import { resourceMessages, useApiResource } from "@/lib/useApiResource";

/** `q` is the submitted search, kept in the URL so Back from a donor record returns to it. */
export default function Staff({ q }: { q: string | null }) {
  // "left" when this person chose to sign out, "expired" when the server ended the session.
  const [signedOut, setSignedOut] = useState<"" | "left" | "expired">("");
  const { resource, data: actor, reload } = useApiResource<Schema<"StaffView">>(signedOut ? null : "/donor/staff/me");
  // Signing out removes the focused button, so focus moves to the way back in.
  const signIn = useRef<HTMLAnchorElement>(null);
  useEffect(() => {
    if (signedOut === "left") signIn.current?.focus();
  }, [signedOut]);
  const [error, setError] = useState("");
  const [invitation, setInvitation] = useState("");
  const [done, setDone] = useState("");
  const [email, setEmail] = useState("");
  const [name, setName] = useState("");
  const [role, setRole] = useState("OP_PARTNERSHIPS");
  const [busy, setBusy] = useState(false);
  const messages = resourceMessages(resource, {
    loading: "Checking staff access…",
    failure: "Staff access could not be loaded right now. Please retry in a moment.",
    forbidden: "This account does not have staff access.",
  });
  async function action(work: () => Promise<void>, outcome = "") {
    if (busy) return;
    setBusy(true);
    setError("");
    setDone("");
    try {
      await work();
      setDone(outcome);
    } catch (cause) {
      if (cause instanceof ApiError && cause.status === 401) setSignedOut("expired");
      else if (cause instanceof ApiError && cause.status === 403)
        setError("Your staff role does not permit this action.");
      else
        setError(failureMessage(cause, "The request was not confirmed. Refresh to check its status before retrying."));
    } finally {
      setBusy(false);
    }
  }
  const manages = actor?.permissions.includes("staff.manage") ?? false;
  const {
    resource: accountList,
    data: accounts,
    reload: reloadAccounts,
  } = useApiResource<Schema<"StaffView">[]>(manages && !signedOut ? "/donor/staff/accounts" : null);
  const accountMessages = resourceMessages(accountList, {
    loading: "Loading staff accounts…",
    failure: "Staff accounts could not be loaded right now. Please retry in a moment.",
  });
  const absent = signedOut !== "" || resource.state === "signedOut";
  return (
    <Page wide eyebrow="Staff" title="DAD staff back office">
      <p>Sandbox donor records. Staff access and actions are audited.</p>
      {absent ? (
        <p>
          <Link className="button" href="/staff/sign-in" ref={signIn}>
            Staff sign-in
          </Link>
        </p>
      ) : (
        actor && (
          <>
            <div className="actions">
              <p>
                Signed in as {actor.name} · {actor.role}
              </p>
              <button
                type="button"
                className="quiet"
                disabled={busy}
                onClick={() =>
                  action(async () => {
                    await api("/donor/staff/sign-out", { method: "POST" });
                    setSignedOut("left");
                  })
                }
              >
                Sign out
              </button>
            </div>
            <nav aria-label="Staff tools" className="links">
              {actor.permissions.includes("data.export") && (
                <Link href="/staff/exports">Exports and enrolment queue</Link>
              )}
              {actor.permissions.includes("content.publish") && (
                <>
                  <Link href="/staff/content">Source documents</Link>
                  <Link href="/staff/content/wording">Badge and milestone wording</Link>
                </>
              )}
              {actor.permissions.includes("initiative.manage") && <Link href="/staff/initiatives">Initiatives</Link>}
              {actor.permissions.includes("event.manage") && <Link href="/staff/events">Events</Link>}
              {actor.permissions.includes("audit.read") && <Link href="/staff/audit">Audit history</Link>}
              {actor.permissions.includes("donor.read") && (
                <>
                  <Link href="/staff/failed-payments">Failed payments</Link>
                  <Link href="/staff/deletions">Account deletions</Link>
                </>
              )}
              {actor.permissions.includes("badge.manage") && (
                <Link href="/staff/founding-claims">Waitlist Founding Donors</Link>
              )}
              {actor.permissions.includes("partner.manage") && (
                <>
                  <Link href="/staff/partners">Partners</Link>
                  <Link href="/staff/partners/pending">Partner applications</Link>
                </>
              )}
            </nav>
            <Form action="/staff" role="search">
              <label>
                Find donor by email
                <input key={q} type="search" name="q" maxLength={320} defaultValue={q ?? ""} />
              </label>
              <button type="submit" className="button">
                Search donors
              </button>
            </Form>
            {q !== null && <DonorSearch key={q} q={q} />}
            {manages && (
              <section>
                <h2>Staff access</h2>
                {accounts && (
                  <ul>
                    {accounts.map((account) => (
                      <li key={account.id}>
                        {account.name} · {account.email} · {account.role} ·{" "}
                        {account.disabled ? "Disabled" : account.active ? "Active" : "Invited"}
                        {account.id !== actor.id && (
                          <>
                            <details>
                              <summary>Replace passkeys or invitation</summary>
                              <p>
                                This removes the staff member’s passkeys and revokes their sessions and invitations.
                                They sign in again only after creating a new passkey from the replacement invitation,
                                which also restores a disabled account.
                              </p>
                              <button
                                type="button"
                                className="secondary danger"
                                disabled={busy}
                                onClick={() =>
                                  action(async () => {
                                    const result = await api<
                                      ResponseBody<"/donor/staff/accounts/{staff_id}/recovery", "post">
                                    >(`/donor/staff/accounts/${account.id}/recovery`, { method: "POST" });
                                    setInvitation(result.invitation_url);
                                    reloadAccounts();
                                  }, `Replacement invitation created for ${account.name}.`)
                                }
                              >
                                Confirm replacement invitation
                              </button>
                            </details>
                            <details>
                              <summary>Change role</summary>
                              <form
                                onSubmit={(event) => {
                                  event.preventDefault();
                                  const chosen = new FormData(event.currentTarget).get("role");
                                  action(async () => {
                                    await api(`/donor/staff/accounts/${account.id}/role`, {
                                      method: "POST",
                                      body: JSON.stringify({ role: chosen }),
                                    });
                                    reloadAccounts();
                                  }, `${account.name}’s role changed to ${chosen}.`);
                                }}
                              >
                                <label>
                                  New role for {account.name}
                                  <select name="role" defaultValue={account.role}>
                                    <option>SUPER_ADMIN</option>
                                    <option>OP_PARTNERSHIPS</option>
                                    <option>OP_GROWTH</option>
                                    <option>OP_IMPACT</option>
                                  </select>
                                </label>
                                <p>A role change ends existing sessions and requires a new sign-in.</p>
                                <button type="submit" className="secondary" disabled={busy}>
                                  Confirm role change
                                </button>
                              </form>
                            </details>
                          </>
                        )}
                        {!account.disabled && account.id !== actor.id && (
                          <details>
                            <summary>Disable account</summary>
                            <p>This immediately revokes staff access.</p>
                            <button
                              type="button"
                              className="secondary danger"
                              disabled={busy}
                              onClick={() =>
                                action(async () => {
                                  await api(`/donor/staff/accounts/${account.id}/disable`, { method: "POST" });
                                  reloadAccounts();
                                }, `${account.name} disabled.`)
                              }
                            >
                              Confirm disable {account.name}
                            </button>
                          </details>
                        )}
                      </li>
                    ))}
                  </ul>
                )}
                <form
                  className="divided"
                  onSubmit={(event) => {
                    event.preventDefault();
                    action(async () => {
                      const result = await api<ResponseBody<"/donor/staff/accounts", "post">>("/donor/staff/accounts", {
                        method: "POST",
                        body: JSON.stringify({ email, name, role }),
                      });
                      setInvitation(result.invitation_url);
                      reloadAccounts();
                    }, "Invitation created.");
                  }}
                >
                  <fieldset disabled={busy}>
                    <legend>Invite individual staff member</legend>
                    <label>
                      Name
                      <input value={name} maxLength={100} onChange={(event) => setName(event.target.value)} required />
                    </label>
                    <label>
                      Email
                      <input
                        type="email"
                        value={email}
                        maxLength={320}
                        onChange={(event) => setEmail(event.target.value)}
                        required
                      />
                    </label>
                    <label>
                      Role
                      <select value={role} onChange={(event) => setRole(event.target.value)}>
                        <option>SUPER_ADMIN</option>
                        <option>OP_PARTNERSHIPS</option>
                        <option>OP_GROWTH</option>
                        <option>OP_IMPACT</option>
                      </select>
                    </label>
                    <button type="submit" className="button">
                      Create private invitation
                    </button>
                  </fieldset>
                </form>
                {invitation && (
                  <p>
                    Share this one-use, 24-hour link through your approved private channel: <code>{invitation}</code>
                  </p>
                )}
              </section>
            )}
            <button type="button" className="secondary" disabled={busy} onClick={reload}>
              Refresh access
            </button>
          </>
        )
      )}
      {(resource.state === "offline" || resource.state === "error") && (
        <button type="button" className="secondary" onClick={reload}>
          Refresh access
        </button>
      )}
      <Announcer
        status={
          absent
            ? signedOut === "left"
              ? "You’ve signed out of DAD staff."
              : ""
            : busy
              ? "Working…"
              : done || messages.status || accountMessages.status
        }
        alert={absent ? "" : error || messages.alert || accountMessages.alert}
      />
    </Page>
  );
}

function DonorSearch({ q }: { q: string }) {
  // Keyset paging: each entry is the cursor that opened a page, the first being none.
  const [cursors, setCursors] = useState<string[]>([]);
  const cursor = cursors.at(-1);
  const query = new URLSearchParams({ q });
  if (cursor) query.set("cursor", cursor);
  const { resource, data, reload } = useApiResource<Schema<"DonorSearch">>(`/donor/staff/donors?${query}`);
  const messages = resourceMessages(resource, {
    loading: "Searching donors…",
    failure: "Donor search could not be completed right now. Please retry in a moment.",
    forbidden: "Your staff role does not permit donor search.",
  });
  return (
    <section aria-labelledby="donor-results">
      <h2 id="donor-results">Donors</h2>
      {resource.state === "signedOut" && (
        <p>
          <Link className="button" href="/staff/sign-in">
            Staff sign-in
          </Link>
        </p>
      )}
      {data && (
        <>
          {data.donors.length ? (
            <ul>
              {data.donors.map((donor) => (
                <li key={donor.id}>
                  <Link href={`/staff/donors/${encodeURIComponent(donor.id)}`}>{donor.email}</Link> ·{" "}
                  {donor.claimed ? "Claimed" : "Guest"}
                  {donor.disabled ? " · Disabled" : ""}
                </li>
              ))}
            </ul>
          ) : (
            <p>No matching donors.</p>
          )}
          {cursors.length > 0 && (
            <button type="button" className="secondary" onClick={() => setCursors(cursors.slice(0, -1))}>
              Previous donor page
            </button>
          )}
          {data.next_cursor !== null && (
            <button
              type="button"
              className="secondary"
              onClick={() => setCursors([...cursors, data.next_cursor ?? ""])}
            >
              Next donor page
            </button>
          )}
        </>
      )}
      {(resource.state === "offline" || resource.state === "error") && (
        <button type="button" className="secondary" onClick={reload}>
          Retry search
        </button>
      )}
      <Announcer status={messages.status} alert={messages.alert} />
    </section>
  );
}
