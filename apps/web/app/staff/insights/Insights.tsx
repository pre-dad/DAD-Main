"use client";
import Link from "next/link";
import { useEffect, useId, useState } from "react";
import { ConfirmButton } from "@/components/ConfirmButton";
import { Announcer } from "@/components/donor/Announcer";
import { KeysetPager } from "@/components/donor/KeysetPager";
import { Page } from "@/components/donor/Page";
import { Insights } from "@/components/insights/Insights";
import { sampleDashboard } from "@/components/insights/sampleDashboard";
import { ApiError, api, failureMessage, type Schema } from "@/lib/api";
import { formatDateTime } from "@/lib/dates";
import { resourceMessages, useApiResource } from "@/lib/useApiResource";
import { useKeysetResource } from "@/lib/useKeysetResource";

type Spec = Schema<"InsightSpec">;
type Problems = { path: string; code: string; message: string }[];

const STATE_LABEL: Record<Spec["state"], string> = {
  draft: "Draft",
  pending: "Waiting for approval",
  live: "Live",
  retired: "Retired",
  rejected: "Rejected",
};
const path = (id: string, action = "") => `/donor/staff/insights/${encodeURIComponent(id)}${action}`;

/** What the API refused: its own explanation, plus every catalog problem with its path and code. */
function problemsOf(cause: unknown): Problems {
  return cause instanceof ApiError && cause.status === 422
    ? (cause.errors as unknown as Problems).filter((item) => typeof item?.code === "string")
    : [];
}

function parseSpec(source: string): { spec: unknown[] } | { error: string } {
  try {
    const value: unknown = JSON.parse(source);
    return Array.isArray(value) ? { spec: value } : { error: "A layout is a JSON array of blocks." };
  } catch {
    return { error: "This is not valid JSON." };
  }
}

function Preview({ source }: { source: string }) {
  const parsed = source.trim() ? parseSpec(source) : null;
  return (
    <section aria-labelledby="preview-heading" className="insights">
      <h3 id="preview-heading">Preview with sample figures</h3>
      {parsed && "error" in parsed && <p className="note">{parsed.error}</p>}
      {parsed && "spec" in parsed && <Insights spec={parsed.spec} dashboard={sampleDashboard} />}
    </section>
  );
}

function ProblemList({ problems }: { problems: Problems }) {
  if (!problems.length) return null;
  return (
    <ul aria-label="Catalog problems">
      {problems.map((problem) => (
        <li key={`${problem.path}:${problem.code}:${problem.message}`}>
          <code>{problem.path || "(whole layout)"}</code> · <code>{problem.code}</code> · {problem.message}
        </li>
      ))}
    </ul>
  );
}

function Detail({ id, me, changed }: { id: string; me: string | undefined; changed: () => void }) {
  const { resource, data, reload } = useApiResource<Spec>(path(id));
  const [source, setSource] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [problems, setProblems] = useState<Problems>([]);
  const [message, setMessage] = useState("");
  const [reason, setReason] = useState("");
  const reasonId = useId();
  const sourceId = useId();
  const loadedSpec = data ? JSON.stringify(data.spec, null, 2) : null;
  const updatedAt = data?.updated_at;
  // biome-ignore lint/correctness/useExhaustiveDependencies: reset the editor only when the saved copy changes
  useEffect(() => {
    if (loadedSpec !== null) setSource(loadedSpec);
  }, [updatedAt, id]);
  const messages = resourceMessages(resource, {
    loading: "Loading layout…",
    failure: "This layout could not be loaded right now. Please retry in a moment.",
    forbidden: "Layouts require the content publishing permission.",
  });
  if (!data) return <Announcer status={messages.status} alert={messages.alert} />;
  const mine = !!me && data.author_id === me;
  const editable = data.state === "draft" && mine;

  async function act(label: string, call: () => Promise<unknown>) {
    setBusy(true);
    setError("");
    setProblems([]);
    setMessage("");
    try {
      await call();
      setMessage(`${label} done.`);
      reload();
      changed();
    } catch (cause) {
      setProblems(problemsOf(cause));
      setError(failureMessage(cause, `${label} was not confirmed. Refresh before retrying.`));
    } finally {
      setBusy(false);
    }
  }
  const post = (action: string, body: unknown = {}) =>
    api<Spec>(path(id, action), { method: "POST", body: JSON.stringify(body) });
  const save = () => {
    const parsed = parseSpec(source);
    if ("error" in parsed) {
      setError(parsed.error);
      return;
    }
    void act("Save", () => post("", { spec: parsed.spec }));
  };
  return (
    <section aria-labelledby={`${reasonId}-title`}>
      <h2 id={`${reasonId}-title`}>
        {STATE_LABEL[data.state]} layout by {data.author_name}
        {mine && " (you)"}
      </h2>
      <p>
        Saved <time dateTime={data.updated_at}>{formatDateTime(data.updated_at)}</time>
        {data.approver_name && (
          <>
            {" "}
            · {data.state === "rejected" ? "Rejected" : "Approved"} by {data.approver_name}
          </>
        )}
        {data.reject_reason && <> · Reason: {data.reject_reason}</>}
      </p>
      <label htmlFor={sourceId}>Layout JSON</label>
      <textarea
        id={sourceId}
        rows={14}
        value={source}
        readOnly={!editable}
        spellCheck={false}
        onChange={(event) => setSource(event.target.value)}
      />
      <ProblemList problems={problems} />
      <div className="actions">
        {editable && (
          <>
            <button type="button" className="secondary" disabled={busy} onClick={save}>
              Save changes
            </button>
            <button type="button" disabled={busy} onClick={() => void act("Submit", () => post("/submit"))}>
              Submit for approval
            </button>
          </>
        )}
        {data.state === "pending" && mine && (
          <p className="note">You wrote this layout, so a different staff member must approve or reject it.</p>
        )}
        {data.state === "pending" && !mine && (
          <>
            <ConfirmButton
              className="button"
              disabled={busy}
              target="this layout"
              prompt="Approving makes this layout live and retires the one now live."
              confirmLabel="Approve and make live"
              onConfirm={() => void act("Approve", () => post("/approve"))}
            >
              Approve
            </ConfirmButton>
            <label htmlFor={reasonId}>Reason for rejecting</label>
            <input id={reasonId} value={reason} maxLength={500} onChange={(event) => setReason(event.target.value)} />
            <ConfirmButton
              disabled={busy || !reason.trim()}
              target="this layout"
              prompt="The author sees the reason and can start a new draft."
              confirmLabel="Reject layout"
              onConfirm={() => void act("Reject", () => post("/reject", { reason: reason.trim() }))}
            >
              Reject
            </ConfirmButton>
          </>
        )}
        {data.state === "live" && (
          <ConfirmButton
            disabled={busy}
            target="the live layout"
            prompt="Donors stop seeing this layout straight away."
            confirmLabel="Retire layout"
            onConfirm={() => void act("Retire", () => post("/retire"))}
          >
            Retire
          </ConfirmButton>
        )}
      </div>
      <Preview source={source} />
      <Announcer status={messages.status || message} alert={error || messages.alert} />
    </section>
  );
}

function NewDraft({ created }: { created: (id: string) => void }) {
  const [source, setSource] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [problems, setProblems] = useState<Problems>([]);
  const sourceId = useId();
  async function save(event: React.FormEvent) {
    event.preventDefault();
    const parsed = parseSpec(source);
    if ("error" in parsed) {
      setError(parsed.error);
      return;
    }
    setBusy(true);
    setError("");
    setProblems([]);
    try {
      const saved = await api<Spec>("/donor/staff/insights", {
        method: "POST",
        body: JSON.stringify({ spec: parsed.spec }),
      });
      setSource("");
      created(saved.id);
    } catch (cause) {
      setProblems(problemsOf(cause));
      setError(failureMessage(cause, "The draft was not confirmed. Refresh the list before retrying."));
    } finally {
      setBusy(false);
    }
  }
  return (
    <form onSubmit={save}>
      <fieldset disabled={busy}>
        <legend>New layout</legend>
        <label htmlFor={sourceId}>Layout JSON</label>
        <textarea
          id={sourceId}
          rows={8}
          value={source}
          spellCheck={false}
          onChange={(event) => setSource(event.target.value)}
        />
        <button type="submit" className="secondary">
          Save as draft
        </button>
      </fieldset>
      <ProblemList problems={problems} />
      <Preview source={source} />
      <p role="alert" className="errorMessage">
        {error}
      </p>
    </form>
  );
}

export default function StaffInsights() {
  const [selected, setSelected] = useState<string | null>(null);
  const { resource: who, data: actor } = useApiResource<Schema<"StaffView">>("/donor/staff/me");
  const { resource, data, reload, heading, newer, older } = useKeysetResource<Schema<"InsightSpecPage">>(
    (cursor) => `/donor/staff/insights${cursor ? `?cursor=${encodeURIComponent(cursor)}` : ""}`,
  );
  const messages = resourceMessages(resource, {
    loading: "Loading layouts…",
    failure: "Layouts could not be loaded right now. Please retry in a moment.",
    forbidden: "Layouts require the content publishing permission.",
  });
  const signedOut = resource.state === "signedOut" || who.state === "signedOut";
  return (
    <Page
      wide
      eyebrow="Staff"
      back={{ href: "/staff", label: "Staff back office" }}
      title="Dashboard insights"
      heading={heading}
    >
      <p>
        A layout is drawn on donors’ dashboards from their own figures. One staff member writes it and a different one
        approves it; only one layout is live at a time.
      </p>
      {signedOut && (
        <p>
          <Link className="button" href="/staff/sign-in">
            Staff sign-in
          </Link>
        </p>
      )}
      {data && (
        <>
          <h2>Layouts</h2>
          {data.specs.length ? (
            <ul>
              {data.specs.map((item) => (
                <li key={item.id}>
                  <button
                    type="button"
                    className="quiet"
                    aria-current={item.id === selected ? "true" : undefined}
                    onClick={() => setSelected(item.id)}
                  >
                    {STATE_LABEL[item.state]} · {item.author_name} · {formatDateTime(item.created_at)}
                  </button>
                </li>
              ))}
            </ul>
          ) : (
            <p>No layouts yet.</p>
          )}
          <KeysetPager newer={newer} older={older} labels={{ newer: "Newer layouts", older: "Older layouts" }} />
          {selected && <Detail key={selected} id={selected} me={actor?.id} changed={reload} />}
          <NewDraft
            created={(id) => {
              reload();
              setSelected(id);
            }}
          />
        </>
      )}
      <Announcer status={messages.status} alert={messages.alert} />
      {!signedOut && (
        <button type="button" className="secondary" onClick={reload}>
          Refresh layouts
        </button>
      )}
    </Page>
  );
}
