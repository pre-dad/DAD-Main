"use client";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { type FormEvent, useEffect, useRef, useState } from "react";
import { ConfirmButton } from "@/components/ConfirmButton";
import { Announcer } from "@/components/donor/Announcer";
import { Page } from "@/components/donor/Page";
import { api, failureMessage, type Schema } from "@/lib/api";
import { formatMoney, parseDollars } from "@/lib/money";
import { resourceMessages, useApiResource } from "@/lib/useApiResource";

type Detail = Schema<"StaffInitiativeDetail">;
type Input = Schema<"InitiativeInput">;

const units: Record<Schema<"MetricInput">["unit"], string> = {
  PEOPLE: "People",
  USD_CENTS: "US dollars",
  INITIATIVES: "Initiatives",
  PARTNERS: "Partners",
};

const field = (form: FormData, name: string) => String(form.get(name) ?? "").trim();

/** The initiative fields from the editor form, or an error to show. */
export function initiativeInput(form: FormData): Input | string {
  const committed = parseDollars(field(form, "committed"));
  if (committed === null) return "Enter the committed amount in dollars.";
  const approvalSource = field(form, "approval_source");
  const approvedOn = field(form, "approved_on");
  if (!approvalSource !== !approvedOn)
    return "Enter both the board-approval source ID and the approval date, or neither.";
  return {
    slug: field(form, "slug"),
    title: field(form, "title"),
    summary: field(form, "summary"),
    type: field(form, "type"),
    location: field(form, "location"),
    region: field(form, "region"),
    counties: field(form, "counties")
      .split(",")
      .map((county) => county.trim())
      .filter(Boolean),
    started_on: field(form, "started_on"),
    status: field(form, "status") === "COMPLETE" ? "COMPLETE" : "ACTIVE",
    committed_minor: committed,
    partner_signup_id: field(form, "partner"),
    board_approval_source_id: approvalSource,
    board_approved_on: approvedOn,
  };
}

export default function InitiativeEditor({ initiative }: { initiative: string | null }) {
  const router = useRouter();
  const id = initiative && encodeURIComponent(initiative);
  const { resource, data, reload } = useApiResource<Detail>(id && `/donor/staff/initiatives/${id}`);
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState("");
  const [error, setError] = useState("");
  // Remount the main form on a reload only after saving it; rotation and publish also bump updated_at.
  const [formVersion, setFormVersion] = useState(0);
  const mainFormSaved = useRef(false);
  // biome-ignore lint/correctness/useExhaustiveDependencies: runs once per loaded record.
  useEffect(() => {
    if (!mainFormSaved.current) return;
    mainFormSaved.current = false;
    setFormVersion((version) => version + 1);
  }, [data]);
  const messages = id
    ? resourceMessages(resource, {
        loading: "Loading initiative…",
        failure: "This initiative could not be loaded right now. Please retry in a moment.",
        forbidden: "Initiatives require Impact Ops access.",
        notFound: "No initiative matches this link.",
      })
    : { status: "", alert: "" };

  async function run(form: HTMLFormElement | null, done: string, action: () => Promise<unknown>) {
    if (busy) return false;
    setBusy(true);
    setStatus("");
    setError("");
    try {
      await action();
      setStatus(done);
      form?.reset();
      if (id) reload();
      return true;
    } catch (cause) {
      setError(failureMessage(cause, "That change could not be saved. Nothing was changed."));
      return false;
    } finally {
      setBusy(false);
    }
  }

  const post = (path: string, body?: unknown) =>
    api<{ id?: string }>(`/donor/staff/initiatives${path}`, {
      method: "POST",
      body: body === undefined ? undefined : JSON.stringify(body),
    });

  function invalid(message: string) {
    setStatus("");
    setError(message);
  }

  function save(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const input = initiativeInput(new FormData(event.currentTarget));
    if (typeof input === "string") {
      invalid(input);
      return;
    }
    if (id) {
      void run(null, "Initiative saved.", () => post(`/${id}`, input)).then((saved) => {
        mainFormSaved.current = saved;
      });
      return;
    }
    void run(null, "Draft initiative created.", async () => {
      const created = await post("", input);
      if (created.id) router.push(`/staff/initiatives/${encodeURIComponent(created.id)}`);
    });
  }

  function saveMetric(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const form = event.currentTarget;
    const values = new FormData(form);
    const unit = field(values, "unit") as Schema<"MetricInput">["unit"];
    const raw = field(values, "value");
    const whole = /^\d+$/.test(raw) && Number.isSafeInteger(Number(raw)) ? Number(raw) : null;
    const value = unit === "USD_CENTS" ? parseDollars(raw) : whole;
    if (value === null) {
      invalid(unit === "USD_CENTS" ? "Enter the metric in dollars." : "Enter the metric as a whole number.");
      return;
    }
    const position = Number(field(values, "position"));
    void run(form, `Metric ${position + 1} saved as a draft.`, () =>
      post(`/${id}/metrics/${position}`, {
        position,
        label: field(values, "label"),
        value,
        unit,
        verification_note: field(values, "note"),
        verified_on: field(values, "verified_on"),
      }),
    );
  }

  function saveSpend(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const form = event.currentTarget;
    const values = new FormData(form);
    const amount = parseDollars(field(values, "amount"));
    if (amount === null) {
      invalid("Enter the category total in dollars.");
      return;
    }
    void run(form, "Spend saved as a draft.", () =>
      post(`/${id}/spend`, {
        category: field(values, "category"),
        amount_minor: amount,
        verification_note: field(values, "verification_note"),
        verified_on: field(values, "spend_verified_on"),
      }),
    );
  }

  const detail = data;
  const ready = !id || detail;
  return (
    <Page
      wide
      eyebrow="Staff"
      back={{ href: "/staff/initiatives", label: "Initiatives" }}
      title={detail?.title ?? (id ? "Initiative" : "New initiative")}
    >
      {resource.state === "signedOut" && id && (
        <p>
          <Link className="button" href="/staff/sign-in">
            Staff sign-in
          </Link>
        </p>
      )}
      {ready && (
        <>
          {detail && (
            <p className="note">
              {detail.publish_state === "PUBLISHED" ? "Published" : "Draft — donors cannot see it yet"}
              {!detail.in_rotation && " · Excluded from the daily rotation"}
            </p>
          )}
          <form onSubmit={save} key={`${detail?.id ?? "new"}-${formVersion}`}>
            <label>
              Slug
              <input name="slug" defaultValue={detail?.slug} readOnly={Boolean(detail)} maxLength={100} required />
            </label>
            <label>
              Title
              <input name="title" defaultValue={detail?.title} maxLength={150} required />
            </label>
            <label>
              Summary
              <textarea name="summary" defaultValue={detail?.summary} maxLength={4000} required />
            </label>
            <label>
              Type
              <input name="type" defaultValue={detail?.type} maxLength={40} required />
            </label>
            <label>
              Location
              <input name="location" defaultValue={detail?.location} maxLength={200} required />
            </label>
            <label>
              Region
              <input name="region" defaultValue={detail?.region} maxLength={100} required />
            </label>
            <label>
              Counties, separated by commas
              <input name="counties" defaultValue={detail?.counties.join(", ")} />
            </label>
            <label>
              Start date
              <input name="started_on" type="date" defaultValue={detail?.started_on} required />
            </label>
            <label>
              Status
              <select name="status" defaultValue={detail?.status ?? "ACTIVE"}>
                <option value="ACTIVE">Active</option>
                <option value="COMPLETE">Complete</option>
              </select>
            </label>
            <label>
              Committed in USD
              <input
                name="committed"
                inputMode="decimal"
                defaultValue={detail ? (detail.committed_minor / 100).toFixed(2) : "0"}
                required
              />
            </label>
            <label>
              Partner ID (optional)
              <input name="partner" defaultValue={detail?.partner_signup_id} />
            </label>
            <fieldset>
              <legend>Board approval</legend>
              <p className="note">Required to publish. Use the ID of the approval document stored under Content.</p>
              <label>
                Approval source ID
                <input
                  name="approval_source"
                  pattern="[0-9a-fA-F-]{36}"
                  defaultValue={detail?.board_approval_source_id}
                />
              </label>
              <label>
                Approved on
                <input name="approved_on" type="date" defaultValue={detail?.board_approved_on} />
              </label>
            </fieldset>
            <button className="button" type="submit" disabled={busy}>
              {detail ? "Save initiative" : "Create draft"}
            </button>
          </form>
          {detail && (
            <>
              <p>
                {detail.publish_state === "DRAFT" && (
                  <ConfirmButton
                    className="button"
                    confirmClassName="button"
                    disabled={busy}
                    prompt={`Publish ${detail.title}? Donors will see it.`}
                    confirmLabel="Confirm publish"
                    onConfirm={() => run(null, "Initiative published.", () => post(`/${id}/publish`))}
                  >
                    Publish initiative
                  </ConfirmButton>
                )}{" "}
                <button
                  type="button"
                  className="secondary"
                  disabled={busy}
                  onClick={() =>
                    run(
                      null,
                      detail.in_rotation ? "Excluded from the daily rotation." : "Included in the daily rotation.",
                      () => post(`/${id}/rotation`, { in_rotation: !detail.in_rotation }),
                    )
                  }
                >
                  {detail.in_rotation ? "Exclude from rotation" : "Include in rotation"}
                </button>
              </p>

              <h2>Headline metrics</h2>
              <p className="note">
                Up to six; the app shows the first four. A metric needs a verification note and date before it can be
                published.
              </p>
              {detail.metrics.length ? (
                <ol>
                  {detail.metrics.map((metric) => (
                    <li key={metric.position} value={metric.position + 1}>
                      {metric.label}:{" "}
                      {metric.unit === "USD_CENTS" ? formatMoney(metric.value) : metric.value.toLocaleString()}{" "}
                      {metric.unit !== "USD_CENTS" && units[metric.unit].toLowerCase()} ·{" "}
                      {metric.publish_state === "PUBLISHED" ? "Published" : "Draft"}
                      {metric.verified_on && ` · Verified ${metric.verified_on}: ${metric.verification_note}`}{" "}
                      {metric.publish_state === "DRAFT" && (
                        <button
                          type="button"
                          className="link"
                          disabled={busy}
                          onClick={() =>
                            run(null, `Metric ${metric.position + 1} published.`, () =>
                              post(`/${id}/metrics/${metric.position}/publish`),
                            )
                          }
                        >
                          Publish metric {metric.position + 1}
                        </button>
                      )}{" "}
                      <ConfirmButton
                        className="link"
                        disabled={busy}
                        prompt={`Delete metric ${metric.position + 1}, ${metric.label}?`}
                        confirmLabel={`Confirm deletion of metric ${metric.position + 1}`}
                        keepLabel={`Keep metric ${metric.position + 1}`}
                        onConfirm={() =>
                          run(null, `Metric ${metric.position + 1} deleted.`, () =>
                            post(`/${id}/metrics/${metric.position}/delete`),
                          )
                        }
                      >
                        Delete metric {metric.position + 1}
                      </ConfirmButton>
                    </li>
                  ))}
                </ol>
              ) : (
                <p>No metrics yet.</p>
              )}
              <form onSubmit={saveMetric}>
                <label>
                  Position
                  <select name="position" defaultValue="0">
                    {[0, 1, 2, 3, 4, 5].map((position) => (
                      <option key={position} value={position}>
                        {position + 1}
                      </option>
                    ))}
                  </select>
                </label>
                <label>
                  Label
                  <input name="label" maxLength={150} required />
                </label>
                <label>
                  Value
                  <input name="value" inputMode="decimal" required />
                </label>
                <label>
                  Unit
                  <select name="unit" defaultValue="PEOPLE">
                    {Object.entries(units).map(([unit, label]) => (
                      <option key={unit} value={unit}>
                        {label}
                      </option>
                    ))}
                  </select>
                </label>
                <label>
                  Verification note
                  <input name="note" maxLength={2000} />
                </label>
                <label>
                  Verified on
                  <input name="verified_on" type="date" />
                </label>
                <button className="button" type="submit" disabled={busy}>
                  Save metric
                </button>
              </form>

              <h2>Spend by category</h2>
              {detail.spend.length ? (
                <ul>
                  {detail.spend.map((row) => (
                    <li key={row.category}>
                      {row.category}: {formatMoney(row.amount_minor)} ·{" "}
                      {row.publish_state === "DRAFT" ? "Draft" : "Published"}{" "}
                      {row.publish_state === "DRAFT" && (
                        <ConfirmButton
                          disabled={busy}
                          className="link"
                          prompt={`Publish ${row.category} spend? Donors will see it.`}
                          confirmLabel={`Confirm publish of ${row.category}`}
                          keepLabel={`Keep ${row.category} as draft`}
                          onConfirm={() =>
                            run(null, "Spend published.", () =>
                              post(`/${id}/spend/publish`, { category: row.category }),
                            )
                          }
                        >
                          Publish {row.category}
                        </ConfirmButton>
                      )}
                      <ConfirmButton
                        className="link"
                        disabled={busy}
                        prompt={`Remove ${row.category} from spend?`}
                        confirmLabel={`Confirm removal of ${row.category}`}
                        keepLabel={`Keep ${row.category}`}
                        onConfirm={() =>
                          run(null, `${row.category} removed.`, () =>
                            post(`/${id}/spend/delete`, { category: row.category }),
                          )
                        }
                      >
                        Remove {row.category}
                      </ConfirmButton>
                    </li>
                  ))}
                </ul>
              ) : (
                <p>No spend recorded.</p>
              )}
              <form onSubmit={saveSpend}>
                <label>
                  Category
                  <input name="category" maxLength={100} required />
                </label>
                <label>
                  Total in USD
                  <input name="amount" inputMode="decimal" required />
                </label>
                <label>
                  Verification note
                  <input name="verification_note" maxLength={2000} />
                </label>
                <label>
                  Verified on
                  <input name="spend_verified_on" type="date" />
                </label>
                <button className="button" type="submit" disabled={busy}>
                  Save spend
                </button>
              </form>
            </>
          )}
        </>
      )}
      <Announcer status={status || messages.status} alert={error || messages.alert} />
      {(resource.state === "offline" || resource.state === "error") && id && (
        <button type="button" className="secondary" onClick={reload}>
          Retry loading
        </button>
      )}
    </Page>
  );
}
