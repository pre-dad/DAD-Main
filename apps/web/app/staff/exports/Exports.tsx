"use client";
import Link from "next/link";
import { useEffect, useRef, useState } from "react";
import { Announcer } from "@/components/donor/Announcer";
import { Page } from "@/components/donor/Page";
import { api, failureMessage, type Schema } from "@/lib/api";
import { formatDateTime } from "@/lib/dates";
import { resourceMessages, useApiResource } from "@/lib/useApiResource";

type Job = Schema<"ExportJob">;

const FIRST_POLL_MS = 1_000;
const LONGEST_POLL_MS = 15_000;

const FAILURES: Record<NonNullable<Job["failure"]>, string> = {
  too_large: "The export is larger than the 50 MB limit. Nothing was kept.",
  forbidden: "Your access changed before the export was built. Nothing was kept.",
  error: "The export could not be built. Request it again, and contact an administrator if it fails again.",
};

function progress(job: Job): string {
  switch (job.state) {
    case "queued":
      return "Waiting to start";
    case "running":
      return `Building · ${job.row_count.toLocaleString("en-US")} rows so far`;
    case "complete":
      return `Ready · ${job.row_count.toLocaleString("en-US")} rows · available until ${formatDateTime(job.expires_at)}`;
    case "failed":
      return `Failed · ${FAILURES[job.failure ?? "error"]}`;
  }
}

const inProgress = (job: Job) => job.state === "queued" || job.state === "running";

export default function Exports() {
  const { resource, data, reload } = useApiResource<Schema<"ExportDatasets">>("/donor/staff/exports");
  const {
    resource: jobsResource,
    data: jobs,
    setData: setJobs,
    reload: reloadJobs,
  } = useApiResource<Schema<"ExportJobs">>("/donor/staff/exports/jobs");
  const [requesting, setRequesting] = useState("");
  const [error, setError] = useState("");
  const [status, setStatus] = useState("");
  const delay = useRef(FIRST_POLL_MS);
  const signature = useRef("");

  // Poll while a file is being built, backing off while nothing changes and
  // starting over whenever progress shows, so a long export is cheap to watch.
  // Each load replaces `jobs`, which schedules the next poll.
  useEffect(() => {
    const state = jobs?.jobs.map((job) => `${job.id}:${job.state}:${job.row_count}`).join(",") ?? "";
    if (state !== signature.current) {
      signature.current = state;
      delay.current = FIRST_POLL_MS;
    }
    if (!jobs?.jobs.some(inProgress)) return;
    const timer = setTimeout(() => {
      delay.current = Math.min(delay.current * 1.5, LONGEST_POLL_MS);
      reloadJobs();
    }, delay.current);
    return () => clearTimeout(timer);
  }, [jobs, reloadJobs]);

  // Announce a file becoming ready or failing, not every row count.
  const outcomes = useRef<Record<string, string>>({});
  useEffect(() => {
    for (const job of jobs?.jobs ?? []) {
      const previous = outcomes.current[job.id];
      outcomes.current[job.id] = job.state;
      if (previous && previous !== job.state && job.state === "complete")
        setStatus(`The ${job.dataset} export is ready.`);
      if (previous && previous !== job.state && job.state === "failed") setError(`The ${job.dataset} export failed.`);
    }
  }, [jobs]);

  async function request(dataset: string) {
    if (requesting) return;
    setRequesting(dataset);
    setError("");
    setStatus("");
    try {
      const job = await api<Job>("/donor/staff/exports/jobs", { method: "POST", body: JSON.stringify({ dataset }) });
      setJobs((current) => ({ jobs: [job, ...current.jobs.filter((item) => item.id !== job.id)] }));
      setStatus(`The ${dataset} export was requested. It builds in the background.`);
      reloadJobs();
    } catch (cause) {
      setError(
        failureMessage(cause, `The ${dataset} export could not be requested right now. Please retry in a moment.`),
      );
    } finally {
      setRequesting("");
    }
  }

  const messages = resourceMessages(resource, {
    loading: "Checking export permissions…",
    failure: "Exports could not be loaded right now. Please retry in a moment.",
    forbidden: "Your role does not permit exports.",
  });
  const listFailed = jobsResource.state === "error" || jobsResource.state === "offline";
  return (
    <Page wide eyebrow="Staff" back={{ href: "/staff", label: "Staff back office" }} title="Exports">
      <p>
        Exports contain private donor information. Store them only in approved locations. Each export records its actor
        and completion status. An export builds in the background, then stays available to you for 24 hours.
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
          <h2>Request an export</h2>
          <ul>
            {data.datasets.map((name) => (
              <li key={name}>
                <button type="button" className="secondary" disabled={requesting !== ""} onClick={() => request(name)}>
                  Request {name} CSV
                </button>
              </li>
            ))}
          </ul>
          <h2>Your exports</h2>
          {jobs?.jobs.length ? (
            <table>
              <thead>
                <tr>
                  <th scope="col">Dataset</th>
                  <th scope="col">Requested</th>
                  <th scope="col">Progress</th>
                  <th scope="col">Download</th>
                </tr>
              </thead>
              <tbody>
                {jobs.jobs.map((job) => (
                  <tr key={job.id}>
                    <th scope="row">{job.dataset}</th>
                    <td>{formatDateTime(job.requested_at)}</td>
                    <td>{progress(job)}</td>
                    <td>
                      {job.state === "complete" && (
                        <a href={`/api/donor/staff/exports/jobs/${job.id}/file`}>Download {job.dataset} CSV</a>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          ) : (
            jobs && <p>You have no exports in the last 24 hours.</p>
          )}
          {listFailed && <p className="errorMessage">Your exports could not be refreshed. They will keep building.</p>}
        </>
      )}
      <h2>Recipient enrolment queue</h2>
      <p>
        Activates at R2. R1 does not collect recipient enrolments. The enrolments export currently contains column
        headers only.
      </p>
      <Announcer status={messages.status || status} alert={messages.alert || error} />
      {resource.state !== "signedOut" && (
        <button
          type="button"
          className="secondary"
          onClick={() => {
            reload();
            reloadJobs();
          }}
        >
          Refresh exports
        </button>
      )}
    </Page>
  );
}
