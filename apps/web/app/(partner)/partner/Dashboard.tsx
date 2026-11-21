"use client";
import Link from "next/link";
import { useId, useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { LogoEditor } from "@/components/donor/LogoEditor";
import { Page } from "@/components/donor/Page";
import { hours, TeamLeaderboard } from "@/components/donor/TeamLeaderboard";
import { api, apiFile, failureMessage, type RequestBody, type ResponseBody, unconfirmedMessage } from "@/lib/api";
import { formatDateTime, formatDay } from "@/lib/dates";
import { formatMoney } from "@/lib/money";
import { resourceMessages, useApiResource } from "@/lib/useApiResource";

type Overview = ResponseBody<"/donor/partner/overview", "get">;
type Initiatives = ResponseBody<"/donor/partner/initiatives", "get">;
type Initiative = Initiatives["initiatives"][number];
type Metric = Initiative["metrics"][number];
type Spend = ResponseBody<"/donor/partner/spend", "get">;
type Users = ResponseBody<"/donor/partner/users", "get">;
type Exports = ResponseBody<"/donor/partner/exports", "get">;
type OurPeople = ResponseBody<"/donor/partner/our-people", "get">;
type Teams = ResponseBody<"/donor/partner/teams", "get">;

const sections = [
  ["overview", "Overview"],
  ["initiatives", "Initiatives"],
  ["spend", "Spend"],
  ["people", "Our People"],
  ["reports", "Reports"],
] as const;

function Tile({ label, value, detail }: { label: string; value: string; detail?: string }) {
  return (
    <article className="metric">
      <h3>{label}</h3>
      <p className="metricValue">{value}</p>
      {detail && <p>{detail}</p>}
    </article>
  );
}

function metricValue(m: Metric): string {
  return m.unit === "USD_CENTS" ? formatMoney(m.value) : m.value.toLocaleString("en-US");
}

/** Deployed against committed, as text and as a bar, so the figure never depends on colour alone. */
function Progress({ deployed, committed }: { deployed: number; committed: number }) {
  const percent = committed > 0 ? Math.min(100, Math.floor((deployed * 100) / committed)) : 0;
  return (
    <div className="progress">
      <p>
        {formatMoney(deployed)} deployed of {formatMoney(committed)} committed ({percent}%)
      </p>
      <div className="progressTrack" aria-hidden="true">
        <div className="progressFill" style={{ width: `${percent}%` }} />
      </div>
    </div>
  );
}

function SpendTable({ rows, caption }: { rows: Spend["categories"]; caption: string }) {
  return (
    // biome-ignore lint/a11y/noNoninteractiveTabindex: a focusable scroll region lets keyboard users pan a wide table.
    <section className="tableScroll" tabIndex={0} aria-label={caption}>
      <table className="dataTable">
        <caption className="visuallyHidden">{caption}</caption>
        <thead>
          <tr>
            <th scope="col">Category</th>
            <th scope="col" className="numeric">
              Total
            </th>
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr key={row.category}>
              <td>{row.category}</td>
              <td className="numeric">{formatMoney(row.amount_minor)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}

function InitiativeCard({ item }: { item: Initiative }) {
  const complete = item.status === "COMPLETE";
  const counties = `${item.county_count} ${item.county_count === 1 ? "county" : "counties"}`;
  const heading = useId();
  return (
    <article className="metric" aria-labelledby={heading}>
      <h3 id={heading} className="initiativeTitle">
        {item.title}
      </h3>
      <p>
        <span className={complete ? "badge" : "badge current"}>{complete ? "Complete" : "Active"}</span>{" "}
        {[item.region, item.county_count > 0 ? counties : ""].filter(Boolean).join(" · ")}
      </p>
      {complete ? (
        <p>Final: {formatMoney(item.deployed_minor)} deployed</p>
      ) : (
        <Progress deployed={item.deployed_minor} committed={item.committed_minor} />
      )}
      {item.metrics.length > 0 && (
        <dl className="details">
          {item.metrics.slice(0, 3).map((m) => (
            <div key={m.label}>
              <dt>{m.label}</dt>
              <dd>{metricValue(m)}</dd>
            </div>
          ))}
        </dl>
      )}
      <details>
        <summary>All figures and spend</summary>
        <p>{item.summary}</p>
        {item.metrics.length > 0 && (
          <ul className="rows">
            {item.metrics.map((m) => (
              <li key={m.label}>
                <span className="rowSummary">
                  {m.label}
                  <br />
                  <span className="note">Verified {formatDay(m.verified_on)}</span>
                </span>
                <strong>{metricValue(m)}</strong>
              </li>
            ))}
          </ul>
        )}
        {item.spend.length > 0 && <SpendTable rows={item.spend} caption={`Spend by category for ${item.title}`} />}
        <p className="note">Last updated {formatDateTime(item.updated_at)}</p>
      </details>
    </article>
  );
}

function InitiativesSection() {
  const { resource, data, failed, reload } = useApiResource<Initiatives>("/donor/partner/initiatives");
  const messages = resourceMessages(resource, {
    loading: "Loading initiatives…",
    failure: "Your initiatives could not be loaded right now.",
  });
  return (
    <section id="initiatives" aria-labelledby="initiatives-heading">
      <h2 id="initiatives-heading">Initiatives</h2>
      {data &&
        (data.initiatives.length ? (
          <div className="metrics">
            {data.initiatives.map((item) => (
              <InitiativeCard key={item.id} item={item} />
            ))}
          </div>
        ) : (
          <p>No published initiatives are linked to your organization yet. DAD links them when they launch.</p>
        ))}
      {failed && (
        <button type="button" className="secondary" onClick={reload}>
          Retry initiatives
        </button>
      )}
      <Announcer status={messages.status} alert={messages.alert} />
    </section>
  );
}

function SpendSection() {
  const { resource, data, failed, reload } = useApiResource<Spend>("/donor/partner/spend");
  const messages = resourceMessages(resource, {
    loading: "Loading spend…",
    failure: "Spend by category could not be loaded right now.",
  });
  return (
    <section id="spend" aria-labelledby="spend-heading">
      <h2 id="spend-heading">Spend</h2>
      <p className="note">
        Totals by category across all of your initiatives. Each initiative’s own totals are in its card.
      </p>
      {data &&
        (data.categories.length ? (
          <SpendTable rows={data.categories} caption="Spend by category across all initiatives" />
        ) : (
          <p>No spend has been recorded for your initiatives yet.</p>
        ))}
      {failed && (
        <button type="button" className="secondary" onClick={reload}>
          Retry spend
        </button>
      )}
      <Announcer status={messages.status} alert={messages.alert} />
    </section>
  );
}

function PeopleSection({ companyCode }: { companyCode: string }) {
  const people = useApiResource<OurPeople>("/donor/partner/our-people");
  const teams = useApiResource<Teams>("/donor/partner/teams");
  const messages = resourceMessages(people.resource, {
    loading: "Loading Our People…",
    failure: "Employee figures could not be loaded right now.",
  });
  const teamMessages = resourceMessages(teams.resource, {
    loading: "Loading teams…",
    failure: "Your team leaderboard could not be loaded right now.",
  });
  const p = people.data;
  return (
    <section id="people" aria-labelledby="people-heading">
      <h2 id="people-heading">Our People</h2>
      {companyCode && (
        <div className="callout highlight">
          <p>
            Your company code is <strong className="hash">{companyCode}</strong>. Share this link, or show the code
            below, so employees can link their giving to your organization:{" "}
            <span className="hash">
              {window.location.origin}/c/{companyCode}
            </span>
          </p>
          {/* biome-ignore lint/performance/noImgElement: a per-partner, same-origin SVG that the image optimizer would only copy */}
          <img
            className="referralCode"
            src="/api/donor/partner/company-code.svg"
            alt={`QR code for company code ${companyCode}`}
            width={240}
            height={240}
          />
          <p className="note">Employees who already give can enter the code under Employer and team.</p>
        </div>
      )}
      {p &&
        (p.suppressed ? (
          <p>
            Employee figures appear once at least 5 employees have linked their giving. Individuals are never shown.
          </p>
        ) : (
          <div className="metrics">
            <Tile
              label="Employees enrolled"
              value={p.enrolled.toLocaleString("en-US")}
              detail={p.workforce_size > 0 ? `of ${p.workforce_size.toLocaleString("en-US")} employees` : undefined}
            />
            <Tile label="Matched giving" value={formatMoney(p.matched_minor)} detail="Your match owed this year" />
            <Tile label="Employee giving" value={formatMoney(p.given_minor)} detail="Since joining" />
            <Tile label="Volunteer hours" value={hours(p.volunteer_minutes)} detail="From event check-ins" />
            <Tile
              label="Teams"
              value={p.teams.toLocaleString("en-US")}
              detail={
                p.streak_verified ? `Average streak ${p.avg_streak_days} days` : "Average streak awaiting verification"
              }
            />
          </div>
        ))}
      {teams.data && teams.data.teams.length > 0 && (
        <>
          <h3>Team leaderboard</h3>
          <TeamLeaderboard teams={teams.data.teams} onRefresh={teams.reload} />
        </>
      )}
      {(people.failed || (people.data && !people.data.suppressed && !people.data.streak_verified)) && (
        <button type="button" className="secondary" onClick={people.reload}>
          {people.failed ? "Retry employee figures" : "Refresh employee figures"}
        </button>
      )}
      {teams.failed && (
        <button type="button" className="secondary" onClick={teams.reload}>
          Retry teams
        </button>
      )}
      <Announcer status={messages.status} alert={messages.alert} />
      <Announcer status={teamMessages.status} alert={teamMessages.alert} />
    </section>
  );
}

function ReportsSection({ admin }: { admin: boolean }) {
  const exports = useApiResource<Exports>("/donor/partner/exports");
  const messages = resourceMessages(exports.resource, {
    loading: "Loading report history…",
    failure: "Your report history could not be loaded right now.",
  });
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState("");
  const [error, setError] = useState("");

  async function downloadReport() {
    setBusy(true);
    setError("");
    setStatus("");
    try {
      const url = URL.createObjectURL(await apiFile("/donor/partner/report.pdf"));
      const link = document.createElement("a");
      link.href = url;
      link.download = "dad-impact-report.pdf";
      link.click();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
      setStatus("Your verified impact report was downloaded.");
      exports.reload();
    } catch (cause) {
      setError(failureMessage(cause, "The report could not be generated right now. Please retry in a moment."));
    } finally {
      setBusy(false);
    }
  }

  return (
    <section id="reports" aria-labelledby="reports-heading">
      <h2 id="reports-heading">Reports</h2>
      <p>
        Verified by DAD. Share it with your board in one click: the report is a branded PDF of this dashboard as it
        stands, with the time it was exported.
      </p>
      {admin ? (
        <button type="button" className="button" onClick={downloadReport} disabled={busy}>
          {busy ? "Preparing report…" : "Export verified report (PDF)"}
        </button>
      ) : (
        <p className="note">Your organization’s partner administrators can export the report.</p>
      )}
      {exports.data &&
        (exports.data.exports.length > 0 ? (
          <>
            <h3>Previous exports</h3>
            <ul className="rows">
              {exports.data.exports.map((row) => (
                <li key={row.id}>
                  <span className="rowSummary">
                    {formatDateTime(row.created_at)}
                    <br />
                    <span className="note">by {row.exported_by}</span>
                  </span>
                  <span>
                    {formatMoney(row.committed_minor)} committed ·{" "}
                    {row.people_reached_verified
                      ? `${row.people_reached.toLocaleString("en-US")} people`
                      : "People reached awaiting verification"}
                  </span>
                  {row.downloadable ? (
                    <a href={`/api/donor/partner/reports/${encodeURIComponent(row.id)}/report.pdf`} download>
                      Download report
                    </a>
                  ) : (
                    <span className="note">File was not retained</span>
                  )}
                </li>
              ))}
            </ul>
          </>
        ) : (
          <p className="note">No reports have been exported yet.</p>
        ))}
      {exports.failed && (
        <button type="button" className="secondary" onClick={exports.reload}>
          Retry report history
        </button>
      )}
      <Announcer status={status || messages.status} alert={error || messages.alert} />
    </section>
  );
}

function UsersSection({ self }: { self: string }) {
  const users = useApiResource<Users>("/donor/partner/users");
  const messages = resourceMessages(users.resource, {
    loading: "Loading users…",
    failure: "Your users could not be loaded right now.",
  });
  const [invite, setInvite] = useState({ email: "", name: "", role: "VIEWER" });
  const [confirming, setConfirming] = useState<string | null>(null);
  const [removing, setRemoving] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState("");
  const [error, setError] = useState("");

  async function sendInvite(event: React.FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError("");
    setStatus("");
    try {
      await api("/donor/partner/users", { method: "POST", body: JSON.stringify(invite) });
      setStatus(`We emailed ${invite.email} an invitation to sign in.`);
      setInvite({ email: "", name: "", role: "VIEWER" });
      users.reload();
    } catch (cause) {
      setError(failureMessage(cause, "The invitation could not be sent right now. Please retry in a moment."));
    } finally {
      setBusy(false);
    }
  }

  async function removeUser(id: string, email: string) {
    if (removing) return;
    setRemoving(id);
    setError("");
    setStatus("");
    try {
      await api(`/donor/partner/users/${encodeURIComponent(id)}/remove`, { method: "POST", body: "{}" });
      setStatus(`Removed ${email}.`);
      setConfirming(null);
      users.reload();
    } catch (cause) {
      setError(failureMessage(cause, "That user could not be removed right now."));
    } finally {
      setRemoving(null);
    }
  }

  async function resendInvite(id: string, email: string) {
    setError("");
    setStatus("");
    try {
      await api(`/donor/partner/users/${id}/reinvite`, { method: "POST", body: "{}" });
      setStatus(`We emailed ${email} a new invitation. The previous link no longer works.`);
    } catch (cause) {
      setError(failureMessage(cause, "The invitation could not be resent right now."));
    }
  }

  return (
    <section id="users" aria-labelledby="users-heading">
      <h2 id="users-heading">Users</h2>
      {users.data && (
        <ul className="rows">
          {users.data.users.map((u) => (
            <li key={u.id}>
              <span className="rowSummary">
                {u.name || u.email}
                <br />
                <span className="note">
                  {u.email} · {u.role === "ADMIN" ? "Administrator" : "Viewer"}
                  {!u.accepted && " · Invitation not yet used"}
                </span>
              </span>
              {u.id === self ? (
                <span className="note">You</span>
              ) : (
                <span className="actions">
                  {!u.accepted && confirming !== u.id && (
                    <button type="button" className="secondary" onClick={() => resendInvite(u.id, u.email)}>
                      Resend invitation <span className="visuallyHidden">to {u.email}</span>
                    </button>
                  )}
                  {confirming === u.id ? (
                    <>
                      <button
                        type="button"
                        className="secondary"
                        disabled={removing !== null}
                        onClick={() => removeUser(u.id, u.email)}
                      >
                        Confirm removal <span className="visuallyHidden">of {u.email}</span>
                      </button>
                      <button
                        type="button"
                        className="secondary"
                        disabled={removing !== null}
                        onClick={() => setConfirming(null)}
                      >
                        Keep <span className="visuallyHidden">{u.email}</span>
                      </button>
                    </>
                  ) : (
                    <button type="button" className="secondary" onClick={() => setConfirming(u.id)}>
                      Remove <span className="visuallyHidden">{u.email}</span>
                    </button>
                  )}
                </span>
              )}
            </li>
          ))}
        </ul>
      )}
      <Announcer status={status || messages.status} alert={error || messages.alert} />
      <form onSubmit={sendInvite}>
        <h3>Invite a colleague</h3>
        <p className="note">
          Viewers can see every dashboard. Administrators can also export reports and manage users.
        </p>
        <fieldset disabled={busy}>
          <label>
            Work email
            <input
              type="email"
              autoComplete="off"
              value={invite.email}
              required
              maxLength={320}
              onChange={(e) => setInvite({ ...invite, email: e.target.value })}
            />
          </label>
          <label>
            Name
            <input
              value={invite.name}
              required
              maxLength={100}
              onChange={(e) => setInvite({ ...invite, name: e.target.value })}
            />
          </label>
          <label>
            Role
            <select value={invite.role} onChange={(e) => setInvite({ ...invite, role: e.target.value })}>
              <option value="VIEWER">Viewer (read-only)</option>
              <option value="ADMIN">Administrator</option>
            </select>
          </label>
        </fieldset>
        <button type="submit" className="button" disabled={busy}>
          {busy ? "Sending…" : "Email invitation"}
        </button>
      </form>
      {users.failed && (
        <button type="button" className="secondary" onClick={users.reload}>
          Retry users
        </button>
      )}
    </section>
  );
}

function OrganizationSection({ overview, onSaved }: { overview: Overview; onSaved: () => void }) {
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState("");
  const [error, setError] = useState("");

  async function save(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    const workforce = Number(form.get("workforce_size"));
    setBusy(true);
    setError("");
    setStatus("");
    try {
      await api("/donor/partner/profile", {
        method: "POST",
        body: JSON.stringify({
          organization: String(form.get("organization") ?? ""),
          description: String(form.get("description") ?? ""),
        } satisfies RequestBody<"/donor/partner/profile", "post">),
      });
    } catch (cause) {
      setError(failureMessage(cause, "The profile could not be saved right now. Please retry in a moment."));
      setBusy(false);
      return;
    }
    try {
      if (workforce !== overview.workforce_size) {
        await api("/donor/partner/workforce", {
          method: "POST",
          body: JSON.stringify({ workforce_size: workforce } satisfies RequestBody<"/donor/partner/workforce", "post">),
        });
      }
      setStatus("Organization profile saved.");
    } catch (cause) {
      setError(failureMessage(cause, "Profile saved; workforce size was not. Please retry in a moment."));
    } finally {
      onSaved();
      setBusy(false);
    }
  }

  return (
    <section id="organization" aria-labelledby="organization-heading">
      <h2 id="organization-heading">Organization</h2>
      <form onSubmit={save} key={`${overview.organization}-${overview.description}-${overview.workforce_size}`}>
        <fieldset disabled={busy}>
          <label>
            Organization name
            <input name="organization" defaultValue={overview.organization} required maxLength={200} />
          </label>
          <label>
            Description
            <textarea name="description" defaultValue={overview.description} maxLength={4000} />
          </label>
          <label>
            Workforce size
            <input
              name="workforce_size"
              type="number"
              min={0}
              step={1}
              inputMode="numeric"
              defaultValue={overview.workforce_size}
              required
            />
          </label>
        </fieldset>
        <p className="note">Workforce size sets the share of employees enrolled on your overview.</p>
        <button type="submit" className="button" disabled={busy}>
          {busy ? "Saving…" : "Save profile"}
        </button>
      </form>
      <LogoEditor
        path="/donor/partner/logo"
        hasLogo={overview.has_logo}
        alt="Your organization logo"
        onSaved={onSaved}
      />
      <Announcer status={status} alert={error} />
    </section>
  );
}

export default function Dashboard() {
  const overview = useApiResource<Overview>("/donor/partner/overview");
  const [signingOut, setSigningOut] = useState(false);
  const [signOutError, setSignOutError] = useState("");
  const o = overview.data;
  const messages = resourceMessages(overview.resource, {
    loading: "Loading your dashboard…",
    failure: "Your partner dashboard could not be loaded right now. Please retry in a moment.",
    forbidden: "This dashboard requires a partner account.",
  });
  const admin = o?.role === "ADMIN";

  async function signOut() {
    if (signingOut) return;
    setSigningOut(true);
    setSignOutError("");
    try {
      await api("/donor/partner/sign-out", { method: "POST", body: "{}" });
      window.location.assign("/partner/sign-in");
    } catch (cause) {
      setSignOutError(unconfirmedMessage(cause, "Sign-out wasn’t confirmed. Please retry before leaving this device."));
    } finally {
      setSigningOut(false);
    }
  }

  return (
    <Page wide eyebrow="Partner portal" title={o?.organization ?? "Partner dashboard"}>
      {overview.resource.state === "signedOut" && (
        <div className="callout">
          <p>Sign in to see your organization’s verified impact.</p>
          <Link className="button" href="/partner/sign-in">
            Email me a sign-in link
          </Link>
        </div>
      )}
      {o && (
        <>
          <p className="note">
            Verified by DAD. Every figure here is published and verified; anything not yet verified is left out, never
            estimated.
            {o.updated_at && <> Figures last updated {formatDateTime(o.updated_at)}.</>}
          </p>
          <nav aria-label="Dashboard sections" className="links">
            {sections.map(([id, label]) => (
              <a key={id} href={`#${id}`}>
                {label}
              </a>
            ))}
            {admin && <a href="#users">Users</a>}
            {admin && <a href="#organization">Organization</a>}
          </nav>
          {o.description && <p className="lead">{o.description}</p>}
          <section id="overview" aria-labelledby="overview-heading">
            <h2 id="overview-heading">Overview</h2>
            <div className="metrics">
              <Tile
                label="Committed"
                value={o.committed_verified ? formatMoney(o.committed_minor) : "Awaiting verification"}
                detail="Across your initiatives"
              />
              <Tile
                label="Deployed"
                value={o.deployed_verified ? formatMoney(o.deployed_minor) : "Awaiting verification"}
                detail={
                  o.deployed_verified && o.committed_verified
                    ? `${o.deployed_percent}% of committed`
                    : "Verified category totals only"
                }
              />
              <Tile
                label="People reached"
                value={o.people_reached_verified ? o.people_reached.toLocaleString("en-US") : "Awaiting verification"}
                detail="Verified figures only"
              />
              <Tile
                label="Employees enrolled"
                value={o.employees_enrolled === null ? "Fewer than 5" : o.employees_enrolled.toLocaleString("en-US")}
                detail={
                  o.employees_enrolled === null
                    ? "Shown once 5 or more employees enroll"
                    : o.enrolled_percent === null
                      ? "Workforce size not yet set"
                      : `${o.enrolled_percent}% of your workforce`
                }
              />
            </div>
          </section>
          <InitiativesSection />
          <SpendSection />
          <PeopleSection companyCode={o.company_code} />
          <ReportsSection admin={admin} />
          {admin && <UsersSection self={o.user_id} />}
          {admin && <OrganizationSection overview={o} onSaved={overview.reload} />}
          <p>
            <button type="button" className="secondary" onClick={signOut} disabled={signingOut}>
              {signingOut ? "Signing out…" : "Sign out"}
            </button>
          </p>
        </>
      )}
      {overview.failed && (
        <button type="button" className="secondary" onClick={overview.reload}>
          Retry dashboard
        </button>
      )}
      <Announcer status={messages.status} alert={signOutError || messages.alert} />
    </Page>
  );
}
