"use client";
import { useRouter } from "next/navigation";
import { useTransition } from "react";

/** Re-renders the current server page, fetching its data again. */
export function RefreshButton({ children }: { children: React.ReactNode }) {
  const router = useRouter();
  const [pending, startTransition] = useTransition();
  return (
    <>
      <button
        type="button"
        className="secondary"
        aria-disabled={pending}
        onClick={() => {
          if (!pending) startTransition(() => router.refresh());
        }}
      >
        {pending ? "Checking again…" : children}
      </button>
      <span role="status" className="visuallyHidden">
        {pending ? "Checking again…" : ""}
      </span>
    </>
  );
}
