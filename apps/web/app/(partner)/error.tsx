"use client";

import { RouteError } from "@/components/donor/RouteError";

export default function PartnerError({ error, retry }: { error: Error & { digest?: string }; retry: () => void }) {
  return (
    <RouteError
      error={error}
      retry={retry}
      lead="Please try again. Before repeating an action, check whether it took effect."
      home={{ href: "/", label: "Back to home" }}
    />
  );
}
