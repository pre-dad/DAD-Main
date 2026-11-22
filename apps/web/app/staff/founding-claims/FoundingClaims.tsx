"use client";
import { type FormEvent, useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { Page } from "@/components/donor/Page";
import { ApiError, api, type FieldError, failureMessage } from "@/lib/api";

// The server rejects bodies over 5 MiB; the CSV travels as a JSON string, so leave 1 MiB for escaping.
const MAX_CSV_BYTES = 4 * 1024 * 1024;

export default function FoundingClaims() {
  const [file, setFile] = useState<File | null>(null);
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState("");
  const [error, setError] = useState("");
  const [rowErrors, setRowErrors] = useState<FieldError[]>([]);

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!file || busy) return;
    if (file.size > MAX_CSV_BYTES) {
      setError("The CSV must be at most 4 MB.");
      return;
    }
    setBusy(true);
    setStatus("");
    setError("");
    setRowErrors([]);
    try {
      const result = await api<{ rows: number }>("/donor/staff/founding-claims", {
        method: "POST",
        body: JSON.stringify({ claims_csv: await file.text() }),
      });
      setStatus(
        `Imported ${result.rows.toLocaleString()} ${result.rows === 1 ? "address" : "addresses"}. Matching donors earn Founding Donor at their next update.`,
      );
    } catch (cause) {
      if (cause instanceof ApiError) setRowErrors(cause.errors);
      setError(failureMessage(cause, "The import was not confirmed. Nothing was stored; check the file and retry."));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Page wide eyebrow="Staff" back={{ href: "/staff", label: "Staff back office" }} title="Waitlist Founding Donors">
      <p>
        Website waitlist sign-ups and earlier website donors are Founding Donors. Import their addresses here; a donor
        whose email matches earns the badge. Only a hash of each address is kept, and the list is deleted 90 days after
        the Founding cutoff.
      </p>
      <form onSubmit={submit}>
        <fieldset disabled={busy}>
          <legend>Import sign-ups</legend>
          <p>CSV columns:</p>
          <pre>email,signed_up_on,source</pre>
          <p>Dates are YYYY-MM-DD, before January 1, 2027. Source is WAITLIST or WEBSITE_DONOR.</p>
          <label>
            Sign-ups CSV
            <input
              type="file"
              accept=".csv,text/csv"
              onChange={(event) => setFile(event.target.files?.[0] || null)}
              required
            />
          </label>
          <button type="submit" className="button">
            Import sign-ups
          </button>
        </fieldset>
      </form>
      <Announcer status={status} alert={error} />
      {rowErrors.length > 0 && (
        <ul className="rows" aria-label="Spreadsheet problems">
          {rowErrors.map((item) => (
            <li key={`${item.row}:${item.column}:${item.reason}`}>
              <span>
                Row {item.row}
                {item.column && (
                  <>
                    , column <code>{item.column}</code>
                  </>
                )}
              </span>
              <span>{item.reason}</span>
            </li>
          ))}
        </ul>
      )}
    </Page>
  );
}
