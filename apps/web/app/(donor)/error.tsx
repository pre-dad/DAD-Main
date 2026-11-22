"use client";

import { RouteError } from "@/components/donor/RouteError";

export default function PageError({ error, retry }: { error: Error & { digest?: string }; retry: () => void }) {
  return (
    <RouteError
      error={error}
      retry={retry}
      lead="Please try again. After a checkout problem, check your giving history before starting another gift."
      home={{ href: "/", label: "Back to home" }}
    />
  );
}
