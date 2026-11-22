"use client";

import { useCallback, useEffect, useState } from "react";

type Link = { token?: string; next?: string };

/**
 * Reads a single-use `#token=` (and optional `next=`) from an emailed link and removes it
 * from the address bar so it never lands in history or a shared screenshot. `token` is
 * undefined until the hash has been read, then the token or "" when the link carried none.
 *
 * With `keep`, the token also survives a reload of this tab until `clear()` is called.
 */
export function useHashToken(keep?: string) {
  const [link, setLink] = useState<Link>({});
  useEffect(() => {
    const params = new URLSearchParams(window.location.hash.slice(1));
    window.history.replaceState(null, "", window.location.pathname + window.location.search);
    let token = params.get("token");
    try {
      if (keep && token) sessionStorage.setItem(keep, token);
      else if (keep) token = sessionStorage.getItem(keep);
    } catch {
      /* Storage may be disabled; the link still works for this load. */
    }
    // StrictMode replays this effect after the hash is gone; keep what the first run found.
    setLink((current) => (current.token ? current : { token: token ?? "", next: params.get("next") ?? undefined }));
  }, [keep]);
  const clear = useCallback(() => {
    setLink({ token: "" });
    try {
      if (keep) sessionStorage.removeItem(keep);
    } catch {
      /* Storage may be disabled; nothing was kept. */
    }
  }, [keep]);
  return { token: link.token, next: link.next, clear };
}
