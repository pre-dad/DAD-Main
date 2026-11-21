// First-party, opt-in observations. Financial truth remains on the server.
const consentKey = "dad.analytics.consent";
const journeyKey = "dad.analytics.journey";
export function analyticsAllowed() {
  try {
    return (
      typeof window !== "undefined" &&
      navigator.doNotTrack !== "1" &&
      !(navigator as Navigator & { globalPrivacyControl?: boolean }).globalPrivacyControl &&
      localStorage.getItem(consentKey) === "yes"
    );
  } catch {
    return false;
  }
}
export function analyticsChoice(value: boolean) {
  try {
    localStorage.setItem(consentKey, value ? "yes" : "no");
    if (!value) sessionStorage.removeItem(journeyKey);
  } catch {
    /* Storage may be disabled. */
  }
}
export function analyticsHasChoice() {
  try {
    return localStorage.getItem(consentKey) !== null;
  } catch {
    return true;
  }
}
export function observe(name: string, properties: Record<string, string> = {}) {
  if (!analyticsAllowed()) return;
  try {
    let journey = sessionStorage.getItem(journeyKey);
    if (!journey) {
      journey = crypto.randomUUID();
      sessionStorage.setItem(journeyKey, journey);
    }
    void fetch("/api/analytics/events", {
      method: "POST",
      credentials: "include",
      keepalive: true,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        events: [
          { id: crypto.randomUUID(), journey_id: journey, name, properties, occurred_at: new Date().toISOString() },
        ],
      }),
    }).catch(() => undefined);
  } catch {
    /* Observations must never block the product. */
  }
}
export function observeAPI(path: string, method: string, result: unknown, body?: BodyInit | null) {
  try {
    sendAPIObservation(path, method, result, body);
  } catch {
    /* Observations must never block the product. */
  }
}
function sendAPIObservation(path: string, method: string, result: unknown, body?: BodyInit | null) {
  if (!analyticsAllowed()) return;
  const data = (result ?? {}) as Record<string, unknown>;
  if (method === "POST") {
    const names: Record<string, string> = {
      "/donor/signup": "signup_started",
      "/donor/claim": "signup_completed",
      "/donor/checkouts": "gift_started",
      "/donor/me/delete": "account_deleted",
      "/donor/me/recurring/cancel": "gift_cancelled",
      "/donor/me/recurring/amount": "amount_changed",
      "/donor/me/team": "team_joined",
      "/donor/partner/applications": "partner_signup_submitted",
    };
    if (names[path]) observe(names[path]);
    if (/\/events\/[^/]+\/reserve$/.test(path))
      observe(data.status === "WAITLISTED" ? "event_waitlisted" : "event_reserved");
    if (path.endsWith("/events/check-in")) observe("event_checked_in");
    if (path === "/donor/me/preferences" && typeof body === "string") {
      try {
        const p = JSON.parse(body);
        if ("rankings_opted_out" in p) observe("rankings_toggled");
        if (p.volunteer) observe("volunteer_opted_in");
      } catch {
        /* Ignore malformed observation context. */
      }
    }
  } else if (/\/(?:me\/)?events\/(?!invite-only$)[^/]+$/.test(path)) observe("event_viewed");
}
