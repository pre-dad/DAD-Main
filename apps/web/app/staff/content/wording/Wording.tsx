"use client";
import Link from "next/link";
import { type FormEvent, useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { Page } from "@/components/donor/Page";
import { api, failureMessage, type Schema } from "@/lib/api";
import { resourceMessages, useApiResource } from "@/lib/useApiResource";
import { Rules } from "./Rules";

type Entry = Schema<"WordingEntry">;
type Kind = "BADGE" | "MILESTONE";

export default function Wording() {
  const { resource, data, reload } = useApiResource<Schema<"RecognitionWording">>("/donor/staff/content/wording");
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState("");
  const [error, setError] = useState("");
  const messages = resourceMessages(resource, {
    loading: "Loading wording…",
    failure: "Badge and milestone wording could not be loaded right now. Please retry in a moment.",
    forbidden: "Editing wording requires content access.",
  });

  async function save(kind: Kind, entry: Entry, title: string, text: string, done: string) {
    if (busy) return;
    setBusy(true);
    setStatus("");
    setError("");
    try {
      await api("/donor/staff/content/wording", {
        method: "POST",
        body: JSON.stringify({ kind, key: entry.key, title, text }),
      });
      setStatus(done);
      reload();
    } catch (cause) {
      setError(failureMessage(cause, "The wording could not be saved. Nothing was changed."));
    } finally {
      setBusy(false);
    }
  }

  function editor(kind: Kind, entry: Entry, label: string, textLabel: string) {
    const submit = (event: FormEvent<HTMLFormElement>) => {
      event.preventDefault();
      const form = new FormData(event.currentTarget);
      const title = String(form.get("title") ?? "").trim();
      const text = String(form.get("text") ?? "").trim();
      if (!title || !text) {
        setStatus("");
        setError(`Enter both a title and text for ${label}. To go back to the standard wording, restore the default.`);
        return;
      }
      return save(kind, entry, title, text, `Saved the wording for ${label}.`);
    };
    return (
      <form key={`${entry.key}:${entry.title}:${entry.text}`} onSubmit={submit}>
        <fieldset disabled={busy}>
          <legend>
            {label}
            {entry.custom && " · Edited"}
          </legend>
          <label>
            Title
            <input name="title" maxLength={60} defaultValue={entry.title} required />
          </label>
          <label>
            {textLabel}
            <input name="text" maxLength={200} defaultValue={entry.text} required />
          </label>
          <button type="submit" className="button">
            Save {label}
          </button>
          {entry.custom && (
            <>
              <p className="note">
                Default: {entry.default_title} — {entry.default_text}
              </p>
              <button
                type="button"
                className="secondary"
                onClick={() => save(kind, entry, "", "", `Restored the default wording for ${label}.`)}
              >
                Restore default for {label}
              </button>
            </>
          )}
        </fieldset>
      </form>
    );
  }

  return (
    <Page wide eyebrow="Staff" back={{ href: "/staff/content", label: "Content" }} title="Badge and milestone wording">
      <p>
        Change the words donors see. Earning thresholds and artwork can be changed below; the yearly milestones after
        the last one listed use the standard wording.
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
          <h2>Badges</h2>
          <p className="note">The description is shown on a badge the donor has not yet earned.</p>
          {data.badges.map((entry) => editor("BADGE", entry, `badge ${entry.key}`, "How to earn it"))}
          <h2>Streak milestones</h2>
          <p className="note">The title and message of the notification sent when a streak reaches the milestone.</p>
          {data.milestones.map((entry) => editor("MILESTONE", entry, `${entry.key}-day milestone`, "Message"))}
        </>
      )}
      <Rules />
      <Announcer status={status || messages.status} alert={error || messages.alert} />
      {(resource.state === "offline" || resource.state === "error") && (
        <button type="button" className="secondary" onClick={reload}>
          Retry loading
        </button>
      )}
    </Page>
  );
}
