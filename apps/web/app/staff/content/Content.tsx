"use client";
import Link from "next/link";
import { useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { Page } from "@/components/donor/Page";
import { ApiError, api, failureMessage, type Schema } from "@/lib/api";
import { resourceMessages, useApiResource } from "@/lib/useApiResource";

export default function Content() {
  const { resource, data, reload } = useApiResource<Schema<"ContentInventory">>("/donor/staff/content");
  const [source, setSource] = useState<File | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [message, setMessage] = useState("");
  const messages = resourceMessages(resource, {
    loading: "Loading source documents…",
    failure: "The source documents could not be loaded right now. Please retry in a moment.",
    forbidden: "Source documents require administrator access.",
  });
  const rejected = (cause: unknown, fallback: string) =>
    cause instanceof ApiError && cause.status === 403
      ? "Your staff role does not permit storing source documents."
      : failureMessage(cause, fallback);
  async function upload(event: React.FormEvent) {
    event.preventDefault();
    if (!source || busy) return;
    const form = event.currentTarget;
    setMessage("");
    if (source.size > 10 * 1024 * 1024) {
      setError("Source documents must be at most 10 MB.");
      return;
    }
    setBusy(true);
    setError("");
    try {
      const result = await api<Schema<"StoredSource">>(
        `/donor/staff/content/sources?name=${encodeURIComponent(source.name)}`,
        { method: "POST", body: source },
      );
      setMessage(`Source stored: ${result.id}. Cite this ID as an initiative's board approval.`);
      form.reset();
      setSource(null);
      reload();
    } catch (cause) {
      setError(
        rejected(
          cause,
          "Source upload was not confirmed. Check source encryption configuration, then refresh the inventory before retrying.",
        ),
      );
    } finally {
      setBusy(false);
    }
  }
  return (
    <Page wide eyebrow="Staff" back={{ href: "/staff", label: "Staff back office" }} title="Source documents">
      <p>
        <Link href="/staff/exports">Exports</Link> ·{" "}
        <Link href="/staff/content/wording">Badge and milestone wording</Link>
      </p>
      <p>
        Retain private source documents, such as a board approval. Initiatives and their spend by category are edited in{" "}
        <Link href="/staff/initiatives">Initiatives</Link>.
      </p>
      {resource.state === "signedOut" && (
        <p>
          <Link className="button" href="/staff/sign-in">
            Staff sign-in
          </Link>
        </p>
      )}
      {data && (
        <>
          <form onSubmit={upload}>
            <fieldset disabled={busy}>
              <legend>Retain a source document</legend>
              <label>
                Source file
                <input type="file" onChange={(event) => setSource(event.target.files?.[0] || null)} required />
              </label>
              <button type="submit" className="secondary">
                Upload private source
              </button>
            </fieldset>
          </form>
          <h2>Source inventory</h2>
          {data.sources.length ? (
            <ul>
              {data.sources.map((item) => (
                <li key={item.id}>
                  <a href={`/api/donor/staff/content/sources/${item.id}`}>{item.name}</a> · ID <code>{item.id}</code> ·
                  SHA-256 <code>{item.sha256}</code>
                </li>
              ))}
            </ul>
          ) : (
            <p>No source documents stored.</p>
          )}
        </>
      )}
      <Announcer status={messages.status || message} alert={error || messages.alert} />
      {resource.state !== "signedOut" && (
        <button type="button" className="secondary" disabled={busy} onClick={reload}>
          Refresh source documents
        </button>
      )}
    </Page>
  );
}
