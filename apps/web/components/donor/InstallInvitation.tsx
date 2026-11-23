"use client";

import { useState } from "react";

export function InstallInvitation({ path }: { path: string }) {
  const [link, setLink] = useState("");
  const [status, setStatus] = useState("");
  const [error, setError] = useState("");

  async function copy() {
    const invitation = new URL(path, window.location.origin).href;
    setLink(invitation);
    setStatus("");
    setError("");
    try {
      await navigator.clipboard.writeText(invitation);
      setStatus("Invitation copied.");
    } catch {
      setError("Copy wasn’t available. Select and copy your invitation below.");
    }
  }

  return (
    <details>
      <summary>Keep this invitation for the app</summary>
      <p>
        Copy this link before installing DAD on your iPhone. In the app, open “Have an invitation?” and paste it before
        creating your account. Keep the link until the app confirms it is saved.
      </p>
      <button type="button" className="secondary" onClick={copy}>
        Copy invitation for the app
      </button>
      {link && (
        <label>
          Your invitation link
          <input type="url" value={link} readOnly onFocus={(event) => event.currentTarget.select()} />
        </label>
      )}
      <p aria-live="polite" aria-atomic="true" className={error ? "errorMessage" : "liveStatus"}>
        {error || status}
      </p>
    </details>
  );
}
