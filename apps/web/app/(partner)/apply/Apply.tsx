"use client";
import Link from "next/link";
import { useRef, useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { Page } from "@/components/donor/Page";
import { api, classifyFailure, failureMessage, unconfirmedMessage } from "@/lib/api";
import { useFocusOnSuccess } from "@/lib/useStepFocus";

const PARTNER_TYPES = ["Corporate", "Nonprofit", "Foundation", "Government", "Other"];

const initial = {
  organization: "",
  website: "",
  type: "Corporate",
  contact_name: "",
  title: "",
  work_email: "",
  phone: "",
  workforce_size: "",
};

export default function Apply() {
  const [data, setData] = useState(initial);
  const [frozen, setFrozen] = useState<Record<string, unknown> | null>(null);
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState(false);
  const heading = useFocusOnSuccess(done);
  const [error, setError] = useState("");
  const key = useRef<string | null>(null);
  const receipt = useRef<string | null>(null);

  async function submit(event: React.FormEvent) {
    event.preventDefault();
    if (busy) return;
    const website =
      data.website.trim() && !/^https?:\/\//i.test(data.website.trim())
        ? `https://${data.website.trim()}`
        : data.website.trim();
    const body = frozen || { ...data, website, workforce_size: Number.parseInt(data.workforce_size, 10) || 0 };
    setFrozen(body);
    key.current ??= crypto.randomUUID();
    receipt.current ??= btoa(String.fromCharCode(...crypto.getRandomValues(new Uint8Array(32))))
      .replaceAll("+", "-")
      .replaceAll("/", "_")
      .replaceAll("=", "");
    setBusy(true);
    setError("");
    try {
      await api("/donor/partner/applications", {
        method: "POST",
        headers: { "Idempotency-Key": key.current },
        body: JSON.stringify({ application: body, receipt: receipt.current }),
      });
      setDone(true);
    } catch (cause) {
      if (classifyFailure(cause) === "rejected") {
        setFrozen(null);
        key.current = null;
        receipt.current = null;
        setError(failureMessage(cause, "Check your answers, then submit again."));
      } else setError(unconfirmedMessage(cause, "Your application is not confirmed. Retry the same form."));
    } finally {
      setBusy(false);
    }
  }

  const field = (name: keyof typeof initial, value: string) => setData({ ...data, [name]: value });

  return (
    <Page eyebrow="Partners" title="Apply to partner with DAD" heading={heading}>
      <p className="lead">
        Tell us about your organization. We review every application; once approved, you’ll receive a sign-in link to
        your partner dashboard.
      </p>
      {!done && (
        <p>
          <Link href="/apply/status">Check an application submitted from this browser</Link>
        </p>
      )}
      {done ? (
        <section className="callout" aria-live="polite">
          <h2>Application received — pending review</h2>
          <p>
            Thank you. Your application for <strong>{frozen?.organization as string}</strong> is{" "}
            <strong>pending</strong> review by the DAD partnerships team. We’ll email{" "}
            <strong>{frozen?.work_email as string}</strong> with a sign-in link when it’s approved, or with next steps
            if we need more information.
          </p>
          <p>
            <Link href="/apply/status">Check application status</Link>
          </p>
        </section>
      ) : (
        <form onSubmit={submit}>
          <fieldset disabled={busy || frozen !== null}>
            <legend>Organization</legend>
            <label>
              Organization name
              <input
                value={data.organization}
                maxLength={200}
                required
                onChange={(e) => field("organization", e.target.value)}
              />
            </label>
            <label>
              Organization type
              <select value={data.type} onChange={(e) => field("type", e.target.value)}>
                {PARTNER_TYPES.map((t) => (
                  <option key={t} value={t}>
                    {t}
                  </option>
                ))}
              </select>
            </label>
            <label>
              Website
              <input
                type="text"
                inputMode="url"
                autoComplete="url"
                placeholder="example.org"
                value={data.website}
                maxLength={500}
                required
                onChange={(e) => field("website", e.target.value)}
              />
            </label>
            <label>
              Approximate workforce size
              <input
                type="number"
                min={0}
                inputMode="numeric"
                value={data.workforce_size}
                required
                onChange={(e) => field("workforce_size", e.target.value)}
              />
            </label>
          </fieldset>
          <fieldset disabled={busy || frozen !== null}>
            <legend>Primary contact</legend>
            <label>
              Your name
              <input
                value={data.contact_name}
                maxLength={100}
                required
                onChange={(e) => field("contact_name", e.target.value)}
              />
            </label>
            <label>
              Your title
              <input value={data.title} maxLength={100} required onChange={(e) => field("title", e.target.value)} />
            </label>
            <label>
              Work email
              <input
                type="email"
                value={data.work_email}
                maxLength={320}
                required
                onChange={(e) => field("work_email", e.target.value)}
              />
            </label>
            <label>
              Phone
              <input
                type="tel"
                autoComplete="tel"
                value={data.phone}
                maxLength={40}
                required
                onChange={(e) => field("phone", e.target.value)}
              />
            </label>
          </fieldset>
          <button className="button" disabled={busy} type="submit">
            {busy ? "Submitting…" : frozen ? "Retry application" : "Submit application"}
          </button>
        </form>
      )}
      <Announcer status="" alert={error} />
    </Page>
  );
}
