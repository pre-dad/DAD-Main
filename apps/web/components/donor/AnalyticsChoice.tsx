"use client";
import { usePathname } from "next/navigation";
import { useEffect, useState } from "react";
import { analyticsAllowed, analyticsChoice, analyticsHasChoice, observe } from "@/lib/analytics";

function emit(path: string) {
  observe("app_opened");
  if (path.startsWith("/r/"))
    observe("attribution_captured", {
      source: new URLSearchParams(location.search).get("via") === "irl" ? "IRL" : "link",
    });
  if (path.startsWith("/c/")) observe("attribution_captured", { source: "company" });
}
export function AnalyticsChoice() {
  const path = usePathname(),
    [ready, setReady] = useState(false),
    [chosen, setChosen] = useState(false);
  useEffect(() => {
    setReady(true);
    setChosen(analyticsHasChoice());
  }, []);
  useEffect(() => {
    if (analyticsAllowed()) emit(path);
  }, [path]);
  if (!ready) return null;
  return (
    <aside aria-label="Usage analytics" className="note analyticsChoice">
      {!chosen ? (
        <>
          <p>
            Allow first-party usage analytics? DAD uses a temporary journey ID to measure signup, sharing and giving. No
            names, email addresses or payment details are sent. Observations are kept for 13 months. You can change this
            choice here.
          </p>
          <div className="actions">
            <button
              className="secondary"
              type="button"
              onClick={() => {
                analyticsChoice(true);
                setChosen(true);
                emit(path);
              }}
            >
              Allow usage analytics
            </button>
            <button
              className="secondary"
              type="button"
              onClick={() => {
                analyticsChoice(false);
                setChosen(true);
              }}
            >
              Decline usage analytics
            </button>
          </div>
        </>
      ) : (
        <button className="quiet" type="button" onClick={() => setChosen(false)}>
          Change analytics preference
        </button>
      )}
    </aside>
  );
}
