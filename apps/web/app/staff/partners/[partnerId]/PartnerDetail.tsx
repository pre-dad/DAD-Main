"use client";
import Link from "next/link";
import { type FormEvent, useEffect, useRef, useState } from "react";
import { ConfirmButton } from "@/components/ConfirmButton";
import { Announcer } from "@/components/donor/Announcer";
import { LogoEditor } from "@/components/donor/LogoEditor";
import { Page } from "@/components/donor/Page";
import { api, classifyFailure, failureMessage, type ResponseBody, type Schema, unconfirmedMessage } from "@/lib/api";
import { formatDollars, formatMoney, parseDollars } from "@/lib/money";
import { taxYear } from "@/lib/taxYear";
import { resourceMessages, useApiResource } from "@/lib/useApiResource";
import { matchTerms } from "../Partners";

type PendingPayment = { key: string; year: number; amount_minor: number };

type MatchStatus = ResponseBody<"/donor/staff/partners/{partner_id}/match-status", "get">;

export default function PartnerDetail({ partner }: { partner: string }) {
  const id = encodeURIComponent(partner);
  const year = taxYear(new Date());
  const { resource, data, reload } = useApiResource(`/donor/staff/partners/${id}`, {
    load: async (signal) => {
      const [detail, match] = await Promise.all([
        api<Schema<"StaffPartnerDetail">>(`/donor/staff/partners/${id}`, { signal }),
        api<MatchStatus>(`/donor/staff/partners/${id}/match-status?year=${year}`, { signal }),
      ]);
      return { detail, match };
    },
  });
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState("");
  const [error, setError] = useState("");
  const [paymentAmount, setPaymentAmount] = useState("");
  const [pendingPayment, setPendingPayment] = useState<PendingPayment | null>(null);
  const paymentRef = useRef<PendingPayment | null>(null);
  const [retryBlocked, setRetryBlocked] = useState(false);
  const paymentStorageKey = `dad:match-payment:${id}`;
  useEffect(() => {
    paymentRef.current = null;
    setPendingPayment(null);
    setRetryBlocked(false);
    setPaymentAmount("");
    try {
      const raw = sessionStorage.getItem(paymentStorageKey);
      if (!raw) return;
      const saved = JSON.parse(raw) as PendingPayment;
      if (
        !/^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$/i.test(saved.key) ||
        !Number.isSafeInteger(saved.year) ||
        !Number.isSafeInteger(saved.amount_minor) ||
        saved.amount_minor <= 0
      )
        throw new Error("Invalid saved payment");
      paymentRef.current = saved;
      setPendingPayment(saved);
      setPaymentAmount((saved.amount_minor / 100).toFixed(2));
    } catch {
      setRetryBlocked(true);
      setError(
        "Saved payment retry details could not be read. Contact an administrator before recording another payment.",
      );
    }
  }, [paymentStorageKey]);

  const messages = resourceMessages(resource, {
    loading: "Loading partner…",
    failure: "This partner could not be loaded right now. Please retry in a moment.",
    forbidden: "Partner administration requires partnerships access.",
    notFound: "No approved partner matches this link.",
  });

  async function run(
    event: FormEvent<HTMLFormElement> | null,
    done: string,
    action: () => Promise<unknown>,
    uncertainFailure?: string,
    discardRefusedPayment?: (cause: unknown) => void,
  ) {
    event?.preventDefault();
    const form = event?.currentTarget;
    if (busy) return;
    setBusy(true);
    setStatus("");
    setError("");
    try {
      await action();
      setStatus(done);
      form?.reset();
      reload();
    } catch (cause) {
      discardRefusedPayment?.(cause);
      setError(
        uncertainFailure
          ? unconfirmedMessage(cause, uncertainFailure)
          : failureMessage(cause, "That change could not be saved. Nothing was changed."),
      );
    } finally {
      setBusy(false);
    }
  }

  const post = (path: string, body: unknown) =>
    api(`/donor/staff/partners/${id}/${path}`, { method: "POST", body: JSON.stringify(body) });

  function saveMatch(event: FormEvent<HTMLFormElement>) {
    const form = new FormData(event.currentTarget);
    const rate = Number(form.get("rate"));
    const cap = parseDollars(String(form.get("cap") ?? ""));
    const workforce = Number(form.get("workforce"));
    if (!Number.isFinite(rate) || rate < 0 || cap === null || !Number.isInteger(workforce) || workforce < 0) {
      event.preventDefault();
      setError("Enter a match rate, a yearly cap in dollars, and a whole-number workforce size.");
      return;
    }
    return run(event, "Match terms saved.", () =>
      post("match", { match_rate_bp: Math.round(rate * 100), match_cap_minor: cap, workforce_size: workforce }),
    );
  }

  function recordPayment(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (busy || retryBlocked) return;
    const amount = parseDollars(paymentAmount);
    if (!amount) {
      setError("Enter the payment received in dollars.");
      return;
    }
    const request = paymentRef.current ?? { key: crypto.randomUUID(), year, amount_minor: amount };
    if (request.amount_minor !== amount) {
      setError("Retry the unconfirmed payment before entering another payment.");
      return;
    }
    try {
      sessionStorage.setItem(paymentStorageKey, JSON.stringify(request));
    } catch {
      setError("This browser cannot save retry details. Use another browser to record the payment safely.");
      return;
    }
    paymentRef.current = request;
    setPendingPayment(request);
    return run(
      event,
      `Recorded a ${formatMoney(request.amount_minor)} match payment for ${request.year}.`,
      async () => {
        const result = await api<{ recorded: boolean }>(`/donor/staff/partners/${id}/match-payment`, {
          method: "POST",
          headers: { "Idempotency-Key": request.key },
          body: JSON.stringify({ year: request.year, amount_minor: request.amount_minor }),
        });
        if (!result.recorded) throw new Error("Payment recording not confirmed");
        sessionStorage.removeItem(paymentStorageKey);
        paymentRef.current = null;
        setPendingPayment(null);
        setPaymentAmount("");
      },
      "The recording is not confirmed. Retry this payment; do not record it again as a new payment.",
      (cause) => {
        // A definite refusal means the server recorded nothing, so the saved retry can go.
        const failure = classifyFailure(cause);
        if (failure !== "rejected" && failure !== "unconfigured") return;
        sessionStorage.removeItem(paymentStorageKey);
        paymentRef.current = null;
        setPendingPayment(null);
      },
    );
  }

  function link(event: FormEvent<HTMLFormElement>) {
    const initiative = String(new FormData(event.currentTarget).get("initiative") ?? "").trim();
    const amount = parseDollars(String(new FormData(event.currentTarget).get("commitment") ?? ""));
    if (amount === null) {
      event.preventDefault();
      setError("Enter this partner’s commitment in dollars.");
      return;
    }
    return run(event, "Initiative linked.", () => post("link", { initiative_id: initiative, committed_minor: amount }));
  }

  function invite(event: FormEvent<HTMLFormElement>) {
    const form = new FormData(event.currentTarget);
    const email = String(form.get("email") ?? "");
    return run(event, `Invited ${email}. They have been emailed a sign-in link.`, () =>
      post("users", { email, name: String(form.get("name") ?? ""), role: String(form.get("role") ?? "VIEWER") }),
    );
  }

  const detail = data?.detail;
  const match = data?.match;
  return (
    <Page
      wide
      eyebrow="Staff"
      back={{ href: "/staff/partners", label: "Partners" }}
      title={detail?.organization ?? "Partner"}
    >
      {resource.state === "signedOut" && (
        <p>
          <Link className="button" href="/staff/sign-in">
            Staff sign-in
          </Link>
        </p>
      )}
      {detail && match && (
        <>
          <p className="note">
            {detail.type || "Partner"}
            {detail.website && (
              <>
                {" "}
                · <a href={detail.website}>{detail.website}</a>
              </>
            )}
            {detail.company_code && (
              <>
                {" "}
                · Company code <code>{detail.company_code}</code>
              </>
            )}
          </p>

          <LogoEditor
            path={`/donor/staff/partners/${id}/logo`}
            hasLogo={detail.has_logo}
            alt={`${detail.organization} logo`}
            onSaved={reload}
            level={2}
            announce={{ status: setStatus, alert: setError }}
          />

          <h2>Matching</h2>
          <p>
            {detail.workforce_size.toLocaleString()} people · {matchTerms(detail.match_rate_bp, detail.match_cap_minor)}
          </p>
          <p>
            {year}: employees gave {formatDollars(match.gifts_minor)} · match owed {formatDollars(match.owed_minor)} ·
            received {formatDollars(match.paid_minor)} · outstanding {formatDollars(match.outstanding_minor)}
          </p>
          <form onSubmit={saveMatch}>
            <label>
              Match rate (%)
              <input
                name="rate"
                inputMode="decimal"
                defaultValue={detail.match_rate_bp / 100}
                key={`rate-${detail.match_rate_bp}`}
                required
              />
            </label>
            <label>
              Annual match cap in USD (0 for no cap)
              <input
                name="cap"
                inputMode="decimal"
                defaultValue={(detail.match_cap_minor / 100).toFixed(2)}
                key={`cap-${detail.match_cap_minor}`}
                required
              />
            </label>
            <label>
              Workforce size
              <input
                name="workforce"
                inputMode="numeric"
                defaultValue={detail.workforce_size}
                key={`workforce-${detail.workforce_size}`}
                required
              />
            </label>
            <button className="button" type="submit" disabled={busy}>
              Save match terms
            </button>
          </form>
          {pendingPayment && (
            <p className="note">
              An unconfirmed {formatMoney(pendingPayment.amount_minor)} payment for {pendingPayment.year} is saved for
              retry. Retrying uses the same request and cannot record it twice.
            </p>
          )}
          <form onSubmit={recordPayment}>
            <label>
              Match payment received for {pendingPayment?.year ?? year} in USD
              <input
                name="amount"
                inputMode="decimal"
                value={paymentAmount}
                onChange={(e) => setPaymentAmount(e.target.value)}
                readOnly={pendingPayment !== null}
                required
              />
            </label>
            <button className="button" type="submit" disabled={busy || retryBlocked}>
              {pendingPayment ? "Retry recorded payment" : "Record payment"}
            </button>
          </form>

          <h2>Initiatives</h2>
          {detail.initiatives.length ? (
            <ul>
              {detail.initiatives.map((initiative) => (
                <li key={initiative.id}>
                  {initiative.title} · {initiative.status}
                  {initiative.publish_state === "DRAFT" && " · Draft"} · Committed{" "}
                  {formatDollars(initiative.committed_minor)}
                </li>
              ))}
            </ul>
          ) : (
            <p>No initiatives are linked to this partner.</p>
          )}
          <form onSubmit={link}>
            <label>
              Initiative ID
              <input name="initiative" required pattern="[0-9a-fA-F-]{36}" />
            </label>
            <label>
              Partner commitment in USD
              <input name="commitment" inputMode="decimal" defaultValue="0" required />
            </label>
            <button className="button" type="submit" disabled={busy}>
              Link initiative
            </button>
          </form>

          <h2>Portal users</h2>
          {detail.users.length ? (
            <ul>
              {detail.users.map((user) => (
                <li key={user.id}>
                  {user.name || user.email} · {user.email} · {user.role === "ADMIN" ? "Admin" : "Viewer"}
                  {!user.accepted && " · Invitation not yet used"}{" "}
                  {!user.accepted && (
                    <>
                      <button
                        type="button"
                        className="link"
                        disabled={busy}
                        onClick={() =>
                          run(null, `Resent the invitation to ${user.email}.`, () =>
                            api(`/donor/staff/partners/${id}/users/${encodeURIComponent(user.id)}/reinvite`, {
                              method: "POST",
                            }),
                          )
                        }
                      >
                        Resend invitation to {user.email}
                      </button>{" "}
                    </>
                  )}
                  <ConfirmButton
                    className="link"
                    disabled={busy}
                    prompt={`Remove ${user.email} from ${detail.organization}'s portal?`}
                    confirmLabel={`Confirm removal of ${user.email}`}
                    keepLabel={`Keep ${user.email}`}
                    onConfirm={() =>
                      run(null, `Removed ${user.email}.`, () =>
                        api(`/donor/staff/partners/${id}/users/${encodeURIComponent(user.id)}/remove`, {
                          method: "POST",
                        }),
                      )
                    }
                  >
                    Remove {user.email}
                  </ConfirmButton>
                </li>
              ))}
            </ul>
          ) : (
            <p>This partner has no active portal users.</p>
          )}
          <form onSubmit={invite}>
            <label>
              Email
              <input name="email" type="email" maxLength={320} required />
            </label>
            <label>
              Name
              <input name="name" maxLength={100} />
            </label>
            <label>
              Role
              <select name="role" defaultValue="VIEWER">
                <option value="VIEWER">Viewer</option>
                <option value="ADMIN">Admin</option>
              </select>
            </label>
            <button className="button" type="submit" disabled={busy}>
              Invite user
            </button>
          </form>
        </>
      )}
      <Announcer status={status || messages.status} alert={error || messages.alert} />
      {(resource.state === "offline" || resource.state === "error") && (
        <button type="button" className="secondary" onClick={reload}>
          Retry loading
        </button>
      )}
    </Page>
  );
}
